package connectors

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	orchestratorsecurity "github.com/migration-platform/orchestrator/internal/security"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// SftpAuth holds credentials loaded from the connector secret.
// Secret may be a bare password string, or JSON:
//
//	{"password":"..."}
//	{"private_key":"-----BEGIN...","passphrase":"optional"}
type SftpAuth struct {
	Password   string
	PrivateKey string
	Passphrase string
}

// ParseSftpAuth interprets the decrypted secret blob.
func ParseSftpAuth(secret string) (SftpAuth, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return SftpAuth{}, errors.New("watched_sftp secret is required (password or private_key JSON)")
	}
	if strings.HasPrefix(secret, "{") {
		var raw map[string]any
		if err := json.Unmarshal([]byte(secret), &raw); err != nil {
			return SftpAuth{}, fmt.Errorf("parse sftp secret JSON: %w", err)
		}
		auth := SftpAuth{}
		if v, ok := raw["password"].(string); ok {
			auth.Password = v
		}
		if v, ok := raw["private_key"].(string); ok {
			auth.PrivateKey = v
		}
		if v, ok := raw["privateKey"].(string); ok && auth.PrivateKey == "" {
			auth.PrivateKey = v
		}
		if v, ok := raw["passphrase"].(string); ok {
			auth.Passphrase = v
		}
		if auth.Password == "" && auth.PrivateKey == "" {
			return SftpAuth{}, errors.New("sftp secret JSON must include password or private_key")
		}
		return auth, nil
	}
	if strings.Contains(secret, "BEGIN") && strings.Contains(secret, "PRIVATE KEY") {
		return SftpAuth{PrivateKey: secret}, nil
	}
	return SftpAuth{Password: secret}, nil
}

// SftpFingerprint builds a stable cursor token from mtime+size (SFTP has no ETag).
func SftpFingerprint(modTime time.Time, size int64) string {
	return fmt.Sprintf("%d:%d", modTime.UTC().Unix(), size)
}

// SftpConfig is the non-secret portion of a watched_sftp connector.
type SftpConfig struct {
	Host                  string
	Port                  int
	Username              string
	Path                  string // remote directory / prefix
	Glob                  string
	Sort                  string
	StagingBucket         string
	StagingPrefix         string
	InsecureIgnoreHostKey bool
	HostKey               string // optional authorized_keys / ssh wire public key
	ConnectTimeout        time.Duration
	MaxFileBytes          int64
	MaxListEntries        int
	SettleAge             time.Duration
	AllowPrivateNetwork   bool
}

// ParseSftpConfig extracts SFTP settings from connector config_json.
func ParseSftpConfig(cfg map[string]any) (SftpConfig, error) {
	out := SftpConfig{
		Port:           22,
		Glob:           "*",
		Sort:           "lexical",
		StagingBucket:  "migration",
		StagingPrefix:  "sftp-landing/",
		ConnectTimeout: 30 * time.Second,
		MaxFileBytes:   1 << 30,
		MaxListEntries: 1000,
		SettleAge:      30 * time.Second,
	}
	if cfg == nil {
		return out, errors.New("watched_sftp config is required")
	}
	out.Host, _ = cfg["host"].(string)
	out.Username, _ = cfg["username"].(string)
	if out.Host == "" || out.Username == "" {
		return out, errors.New("watched_sftp requires host and username")
	}
	switch p := cfg["port"].(type) {
	case float64:
		out.Port = int(p)
	case int:
		out.Port = p
	case string:
		if n, err := strconv.Atoi(p); err == nil {
			out.Port = n
		}
	}
	if out.Port <= 0 {
		out.Port = 22
	}
	if v, ok := cfg["path"].(string); ok && v != "" {
		out.Path = v
	} else if v, ok := cfg["prefix"].(string); ok {
		out.Path = v
	}
	if v, ok := cfg["glob"].(string); ok && v != "" {
		out.Glob = v
	}
	if v, ok := cfg["sort"].(string); ok && v != "" {
		out.Sort = v
	}
	if v, ok := cfg["staging_bucket"].(string); ok && v != "" {
		out.StagingBucket = v
	} else if v, ok := cfg["bucket"].(string); ok && v != "" {
		out.StagingBucket = v
	}
	if v, ok := cfg["staging_prefix"].(string); ok {
		out.StagingPrefix = v
	}
	if v, ok := cfg["host_key"].(string); ok {
		out.HostKey = v
	} else if v, ok := cfg["hostKey"].(string); ok {
		out.HostKey = v
	}
	if v, ok := cfg["insecure_ignore_host_key"].(bool); ok {
		out.InsecureIgnoreHostKey = v
	} else if v, ok := cfg["insecureIgnoreHostKey"].(bool); ok {
		out.InsecureIgnoreHostKey = v
	}
	out.MaxFileBytes = int64Config(cfg, "max_file_bytes", out.MaxFileBytes)
	out.MaxListEntries = int(int64Config(cfg, "max_list_entries", int64(out.MaxListEntries)))
	out.SettleAge = time.Duration(int64Config(cfg, "settle_seconds", int64(out.SettleAge/time.Second))) * time.Second
	if out.MaxFileBytes <= 0 || out.MaxListEntries <= 0 || out.MaxListEntries > 10000 || out.SettleAge < 0 {
		return out, errors.New("invalid SFTP limits")
	}
	out.AllowPrivateNetwork = os.Getenv("ALLOW_PRIVATE_DESTINATIONS") == "true"
	return out, nil
}

func int64Config(cfg map[string]any, key string, def int64) int64 {
	switch v := cfg[key].(type) {
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case string:
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

// StagingObjectKey builds the MinIO key used after SFTP download.
func StagingObjectKey(stagingPrefix string, connectorID int64, remoteKey string) string {
	prefix := strings.Trim(stagingPrefix, "/")
	// Strip leading slashes so path.Join does not treat remote as absolute.
	remote := strings.TrimLeft(strings.ReplaceAll(remoteKey, "\\", "/"), "/")
	if prefix == "" {
		return path.Join(fmt.Sprintf("sftp/%d", connectorID), remote)
	}
	return path.Join(prefix, fmt.Sprintf("%d", connectorID), remote)
}

// FilterAndSortObjects applies basename/full-key glob and sort — shared by S3 and SFTP watches.
func FilterAndSortObjects(in []ObjectInfo, globPattern, sortBy string) ([]ObjectInfo, error) {
	if globPattern == "" {
		globPattern = "*"
	}
	if sortBy == "" {
		sortBy = "lexical"
	}
	out := make([]ObjectInfo, 0, len(in))
	for _, obj := range in {
		if strings.HasSuffix(obj.Key, "/") {
			continue
		}
		base := path.Base(obj.Key)
		ok, err := path.Match(globPattern, base)
		if err != nil {
			return nil, err
		}
		if !ok {
			ok, err = path.Match(globPattern, obj.Key)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		out = append(out, obj)
	}
	switch sortBy {
	case "mtime":
		sort.Slice(out, func(i, j int) bool {
			if out[i].LastModified.Equal(out[j].LastModified) {
				return out[i].Key < out[j].Key
			}
			return out[i].LastModified.Before(out[j].LastModified)
		})
	default:
		sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	}
	return out, nil
}

type sftpSession struct {
	sshClient  *ssh.Client
	sftpClient *sftp.Client
}

func dialSFTP(cfg SftpConfig, auth SftpAuth) (*sftpSession, error) {
	hostKeyCallback, err := buildHostKeyCallback(cfg)
	if err != nil {
		return nil, err
	}
	sshCfg := &ssh.ClientConfig{
		User:            cfg.Username,
		HostKeyCallback: hostKeyCallback,
		Timeout:         cfg.ConnectTimeout,
	}
	if auth.PrivateKey != "" {
		signer, err := parsePrivateKey(auth.PrivateKey, auth.Passphrase)
		if err != nil {
			return nil, err
		}
		sshCfg.Auth = []ssh.AuthMethod{ssh.PublicKeys(signer)}
	} else {
		sshCfg.Auth = []ssh.AuthMethod{ssh.Password(auth.Password)}
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	dialer := &net.Dialer{Timeout: cfg.ConnectTimeout, KeepAlive: 30 * time.Second}
	if !cfg.AllowPrivateNetwork {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, splitErr := net.SplitHostPort(address)
			if splitErr != nil {
				return splitErr
			}
			if orchestratorsecurity.IPBlocked(net.ParseIP(host)) {
				return fmt.Errorf("SFTP destination %s is in a blocked private/internal range", host)
			}
			return nil
		}
	}
	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("sftp dial %s: %w", addr, err)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, sshCfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("sftp SSH handshake %s: %w", addr, err)
	}
	sshClient := ssh.NewClient(sshConn, chans, reqs)
	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		_ = sshClient.Close()
		return nil, fmt.Errorf("sftp client: %w", err)
	}
	return &sftpSession{sshClient: sshClient, sftpClient: sftpClient}, nil
}

func (s *sftpSession) Close() {
	if s.sftpClient != nil {
		_ = s.sftpClient.Close()
	}
	if s.sshClient != nil {
		_ = s.sshClient.Close()
	}
}

func parsePrivateKey(pem, passphrase string) (ssh.Signer, error) {
	if passphrase != "" {
		return ssh.ParsePrivateKeyWithPassphrase([]byte(pem), []byte(passphrase))
	}
	return ssh.ParsePrivateKey([]byte(pem))
}

func buildHostKeyCallback(cfg SftpConfig) (ssh.HostKeyCallback, error) {
	if cfg.HostKey != "" {
		key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(cfg.HostKey))
		if err != nil {
			if raw, berr := base64.StdEncoding.DecodeString(strings.TrimSpace(cfg.HostKey)); berr == nil {
				if pk, perr := ssh.ParsePublicKey(raw); perr == nil {
					return ssh.FixedHostKey(pk), nil
				}
			}
			return nil, fmt.Errorf("parse host_key: %w", err)
		}
		return ssh.FixedHostKey(key), nil
	}
	if cfg.InsecureIgnoreHostKey {
		return ssh.InsecureIgnoreHostKey(), nil
	}
	return nil, errors.New("watched_sftp requires config.host_key or insecure_ignore_host_key=true (dev only)")
}

// ListSftpObjects connects, walks the remote path recursively, filters, and sorts.
func ListSftpObjects(ctx context.Context, cfg SftpConfig, auth SftpAuth) ([]ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sess, err := dialSFTP(cfg, auth)
	if err != nil {
		return nil, err
	}
	defer sess.Close()

	root := cfg.Path
	if root == "" {
		root = "."
	}
	var raw []ObjectInfo
	walker := sess.sftpClient.Walk(root)
	for walker.Step() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := walker.Err(); err != nil {
			return nil, err
		}
		info := walker.Stat()
		if info == nil || info.IsDir() {
			continue
		}
		if info.Size() > cfg.MaxFileBytes || time.Since(info.ModTime()) < cfg.SettleAge {
			continue
		}
		key := walker.Path()
		// Normalize to forward-slash relative-looking keys.
		key = strings.ReplaceAll(key, "\\", "/")
		mod := info.ModTime()
		raw = append(raw, ObjectInfo{
			Key:          key,
			ETag:         SftpFingerprint(mod, info.Size()),
			Size:         info.Size(),
			LastModified: mod.UTC(),
		})
		if len(raw) >= cfg.MaxListEntries {
			break
		}
	}
	return FilterAndSortObjects(raw, cfg.Glob, cfg.Sort)
}

// DownloadSftpFile writes a remote path to destPath (local), enforcing maxBytes (0 = no cap).
func DownloadSftpFile(ctx context.Context, cfg SftpConfig, auth SftpAuth, remoteKey, destPath, expectedFingerprint string, expectedSize int64) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	sess, err := dialSFTP(cfg, auth)
	if err != nil {
		return 0, err
	}
	defer sess.Close()
	before, err := sess.sftpClient.Stat(remoteKey)
	if err != nil {
		return 0, fmt.Errorf("sftp pre-download stat %s: %w", remoteKey, err)
	}
	if before.IsDir() || before.Size() > cfg.MaxFileBytes || (expectedSize > 0 && before.Size() != expectedSize) ||
		(expectedFingerprint != "" && SftpFingerprint(before.ModTime(), before.Size()) != expectedFingerprint) {
		return 0, fmt.Errorf("remote file changed or exceeds configured size limit before download")
	}

	src, err := sess.sftpClient.Open(remoteKey)
	if err != nil {
		return 0, fmt.Errorf("sftp open %s: %w", remoteKey, err)
	}
	defer src.Close()

	if err := os.MkdirAll(path.Dir(destPath), 0o750); err != nil {
		return 0, err
	}
	dst, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return 0, err
	}
	defer dst.Close()

	var r io.Reader = src
	r = io.LimitReader(src, cfg.MaxFileBytes+1)
	n, err := io.Copy(dst, r)
	if err != nil {
		return n, err
	}
	if n > cfg.MaxFileBytes {
		_ = os.Remove(destPath)
		return n, fmt.Errorf("remote file size exceeds limit %d", cfg.MaxFileBytes)
	}
	after, err := sess.sftpClient.Stat(remoteKey)
	if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || n != before.Size() {
		_ = os.Remove(destPath)
		return n, fmt.Errorf("remote file changed during download")
	}
	return n, nil
}
