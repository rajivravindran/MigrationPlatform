package connectors

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseSftpAuthPasswordPlain(t *testing.T) {
	auth, err := ParseSftpAuth("hunter2")
	require.NoError(t, err)
	require.Equal(t, "hunter2", auth.Password)
}

func TestParseSftpAuthJSON(t *testing.T) {
	auth, err := ParseSftpAuth(`{"private_key":"-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----","passphrase":"x"}`)
	require.NoError(t, err)
	require.Contains(t, auth.PrivateKey, "BEGIN OPENSSH")
	require.Equal(t, "x", auth.Passphrase)
}

func TestParseSftpAuthRequiresCredential(t *testing.T) {
	_, err := ParseSftpAuth(`{"passphrase":"only"}`)
	require.Error(t, err)
	_, err = ParseSftpAuth("")
	require.Error(t, err)
}

func TestParseSftpConfigDefaults(t *testing.T) {
	cfg, err := ParseSftpConfig(map[string]any{
		"host": "sftp.example.com", "username": "drop",
		"prefix": "/incoming", "glob": "*.csv",
		"insecure_ignore_host_key": true,
	})
	require.NoError(t, err)
	require.Equal(t, 22, cfg.Port)
	require.Equal(t, "/incoming", cfg.Path)
	require.Equal(t, "*.csv", cfg.Glob)
	require.Equal(t, "migration", cfg.StagingBucket)
	require.True(t, cfg.InsecureIgnoreHostKey)
	require.Equal(t, int64(1<<30), cfg.MaxFileBytes)
	require.Equal(t, 1000, cfg.MaxListEntries)
	require.Equal(t, 30*time.Second, cfg.SettleAge)
}

func TestParseSftpConfigBounds(t *testing.T) {
	cfg, err := ParseSftpConfig(map[string]any{
		"host": "sftp.example.com", "username": "drop",
		"max_file_bytes": "4096", "max_list_entries": float64(25), "settle_seconds": float64(60),
	})
	require.NoError(t, err)
	require.Equal(t, int64(4096), cfg.MaxFileBytes)
	require.Equal(t, 25, cfg.MaxListEntries)
	require.Equal(t, time.Minute, cfg.SettleAge)

	_, err = ParseSftpConfig(map[string]any{
		"host": "sftp.example.com", "username": "drop", "max_list_entries": float64(10001),
	})
	require.Error(t, err)
}

func TestParseSftpConfigRequiresHostUser(t *testing.T) {
	_, err := ParseSftpConfig(map[string]any{"host": "x"})
	require.Error(t, err)
}

func TestSftpFingerprintStable(t *testing.T) {
	ts := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	a := SftpFingerprint(ts, 100)
	b := SftpFingerprint(ts.In(time.FixedZone("x", 3600)), 100)
	require.Equal(t, a, b)
	require.Equal(t, SftpFingerprint(ts.UTC(), 100), a)
	require.NotEqual(t, a, SftpFingerprint(ts, 101))
}

func TestFilterAndSortObjectsGlobAndMtime(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	in := []ObjectInfo{
		{Key: "/incoming/b.csv", ETag: "1", Size: 1, LastModified: t1},
		{Key: "/incoming/a.csv", ETag: "2", Size: 2, LastModified: t0},
		{Key: "/incoming/skip.txt", ETag: "3", Size: 3, LastModified: t0},
		{Key: "/incoming/nested/c.csv", ETag: "4", Size: 4, LastModified: t0},
	}
	out, err := FilterAndSortObjects(in, "*.csv", "mtime")
	require.NoError(t, err)
	require.Len(t, out, 3)
	require.Equal(t, "/incoming/a.csv", out[0].Key)
	require.Equal(t, "/incoming/nested/c.csv", out[1].Key)
	require.Equal(t, "/incoming/b.csv", out[2].Key)
}

func TestStagingObjectKey(t *testing.T) {
	require.Equal(t, "sftp-landing/9/incoming/a.csv", StagingObjectKey("sftp-landing/", 9, "incoming/a.csv"))
	require.Equal(t, "sftp-landing/9/incoming/a.csv", StagingObjectKey("sftp-landing/", 9, "/incoming/a.csv"))
	require.Equal(t, "sftp/9/a.csv", StagingObjectKey("", 9, "/a.csv"))
}

func TestBuildHostKeyCallbackRequiresExplicitTrust(t *testing.T) {
	_, err := buildHostKeyCallback(SftpConfig{Host: "h", Username: "u"})
	require.Error(t, err)
	cb, err := buildHostKeyCallback(SftpConfig{InsecureIgnoreHostKey: true})
	require.NoError(t, err)
	require.NotNil(t, cb)
}
