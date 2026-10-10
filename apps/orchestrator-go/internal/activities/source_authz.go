package activities

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/temporal"

	"github.com/migration-platform/orchestrator/internal/connectors"
	"github.com/migration-platform/orchestrator/internal/security"
)

// Error types surfaced to workflows; both are non-retryable because a retry
// cannot change ownership or bring back the original bytes.
const (
	ErrTypeSourceNotAuthorized = "SourceNotAuthorized"
	ErrTypeSourceChanged       = "SourceChanged"
)

// Query options a connector-backed job may set; everything else (endpoints,
// client id, credentials) comes from the stored connector.
var connectorQueryKeys = []string{"soql", "cursorField", "cursor"}

type registeredSource struct {
	ID     int64
	Bucket string
	Key    string
	ETag   string
	Size   int64
}

type storedConnector struct {
	Kind   string
	Config map[string]any
	Secret string
}

// sourceStore reads the org-owned source registry and connectors (ADR 0001).
// Lookups return pgx.ErrNoRows when the row is missing or owned by another org.
type sourceStore interface {
	SourceByID(ctx context.Context, orgID, id int64) (registeredSource, error)
	SourceByObject(ctx context.Context, orgID int64, bucket, key string) (registeredSource, error)
	Connector(ctx context.Context, orgID, id int64) (storedConnector, error)
}

type statFunc func(ctx context.Context, bucket, key string) (connectors.ObjectInfo, error)

type sourceAuthorizer struct {
	store           sourceStore
	stat            statFunc
	allowLocalPaths bool
	// allowedBuckets may hold watched or staged objects; the platform's own
	// credentials are used there, so the set is operator-controlled.
	allowedBuckets map[string]bool
}

func newSourceAuthorizer(db *pgxpool.Pool, masterKey string) *sourceAuthorizer {
	return &sourceAuthorizer{
		store:           pgSourceStore{db: db, masterKey: masterKey},
		stat:            connectors.StatObject,
		allowLocalPaths: os.Getenv("ALLOW_LOCAL_SOURCE_PATHS") == "true",
		allowedBuckets:  allowedBucketsFromEnv(),
	}
}

func allowedBucketsFromEnv() map[string]bool {
	platform := os.Getenv("MINIO_BUCKET")
	if platform == "" {
		platform = "migration"
	}
	out := map[string]bool{platform: true}
	for _, b := range strings.Split(os.Getenv("WATCH_ALLOWED_BUCKETS"), ",") {
		if b = strings.TrimSpace(b); b != "" {
			out[b] = true
		}
	}
	return out
}

// OrgObjectPrefix is the only object namespace an organization can configure
// (watched prefixes, SFTP staging). Platform-generated keys live elsewhere.
func OrgObjectPrefix(orgID int64) string { return fmt.Sprintf("orgs/%d/", orgID) }

// checkOrgLocation refuses watched/staged keys or prefixes outside the org's
// namespace. Callers pass the final, cleaned key where one exists so names
// supplied by remote servers cannot traverse out of the prefix.
func (s *sourceAuthorizer) checkOrgLocation(orgID int64, bucket, keyOrPrefix string) error {
	if orgID <= 0 {
		return notAuthorized("object location %s/%s has no owning organization", bucket, keyOrPrefix)
	}
	if !s.allowedBuckets[bucket] {
		return notAuthorized("bucket %q is not allowed for watched or staged objects (see WATCH_ALLOWED_BUCKETS)", bucket)
	}
	p := strings.TrimLeft(keyOrPrefix, "/")
	if strings.Contains(p, "\\") || slices.Contains(strings.Split(p, "/"), "..") {
		return notAuthorized("object location %q is not a clean path", keyOrPrefix)
	}
	if !strings.HasPrefix(p, OrgObjectPrefix(orgID)) {
		return notAuthorized("object location %q is outside %s", keyOrPrefix, OrgObjectPrefix(orgID))
	}
	return nil
}

func notAuthorized(format string, args ...any) error {
	return temporal.NewNonRetryableApplicationError(fmt.Sprintf(format, args...), ErrTypeSourceNotAuthorized, nil)
}

// authorize turns workflow-supplied ingest input into a connector config the
// caller's organization is allowed to read. Workflow input is treated as
// untrusted: it may predate the registry or have been built by an older API.
func (s *sourceAuthorizer) authorize(ctx context.Context, in IngestInput) (connectors.Config, error) {
	cfg := in.ConnectorConfig
	if in.ConnectorID > 0 {
		return s.connectorSource(ctx, in)
	}
	if in.ConnectorKind == "salesforce" || in.ConnectorSecret != "" {
		return connectors.Config{}, notAuthorized("connector sources must reference a stored connector; inline credentials are not accepted")
	}
	bucket, _ := cfg["s3_bucket"].(string)
	key, _ := cfg["s3_key"].(string)
	if bucket != "" && key != "" {
		return s.objectSource(ctx, in, bucket, key)
	}
	if _, ok := cfg["path"]; ok {
		if !s.allowLocalPaths {
			return connectors.Config{}, notAuthorized("local file sources are disabled (ALLOW_LOCAL_SOURCE_PATHS is not set)")
		}
		return connectors.Config{Kind: in.ConnectorKind, Config: cfg}, nil
	}
	return connectors.Config{}, notAuthorized("source is not a registered object or stored connector")
}

func (s *sourceAuthorizer) objectSource(ctx context.Context, in IngestInput, bucket, key string) (connectors.Config, error) {
	switch in.ConnectorKind {
	case "csv", "json", "xml":
	default:
		return connectors.Config{}, notAuthorized("unsupported object format %q", in.ConnectorKind)
	}
	for _, k := range []string{"url", "path"} {
		if _, ok := in.ConnectorConfig[k]; ok {
			return connectors.Config{}, notAuthorized("object sources cannot also set %q", k)
		}
	}

	var (
		src registeredSource
		err error
	)
	if in.SourceID > 0 {
		src, err = s.store.SourceByID(ctx, in.OrgID, in.SourceID)
	} else {
		src, err = s.store.SourceByObject(ctx, in.OrgID, bucket, key)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return connectors.Config{}, notAuthorized("source %s/%s is not registered for org %d", bucket, key, in.OrgID)
	}
	if err != nil {
		return connectors.Config{}, fmt.Errorf("load source: %w", err)
	}
	if src.Bucket != bucket || src.Key != key {
		return connectors.Config{}, notAuthorized("source %d does not name %s/%s", src.ID, bucket, key)
	}

	info, err := s.stat(ctx, bucket, key)
	if err != nil {
		return connectors.Config{}, fmt.Errorf("stat source %d: %w", src.ID, err)
	}
	if src.ETag != "" && info.ETag != src.ETag {
		return connectors.Config{}, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("source %d changed since registration (etag %s, now %s)", src.ID, src.ETag, info.ETag),
			ErrTypeSourceChanged, nil)
	}
	if src.Size > 0 && info.Size != src.Size {
		return connectors.Config{}, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("source %d changed since registration (size %d, now %d)", src.ID, src.Size, info.Size),
			ErrTypeSourceChanged, nil)
	}
	return connectors.Config{Kind: in.ConnectorKind, Config: in.ConnectorConfig}, nil
}

func (s *sourceAuthorizer) connectorSource(ctx context.Context, in IngestInput) (connectors.Config, error) {
	conn, err := s.store.Connector(ctx, in.OrgID, in.ConnectorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return connectors.Config{}, notAuthorized("connector %d not found for org %d", in.ConnectorID, in.OrgID)
	}
	if err != nil {
		return connectors.Config{}, fmt.Errorf("load connector: %w", err)
	}
	if conn.Kind != "salesforce" {
		return connectors.Config{}, notAuthorized("connector %d kind %q cannot feed a job directly", in.ConnectorID, conn.Kind)
	}
	merged := make(map[string]any, len(conn.Config)+len(connectorQueryKeys))
	for k, v := range conn.Config {
		merged[k] = v
	}
	for _, k := range connectorQueryKeys {
		if v, ok := in.ConnectorConfig[k]; ok {
			merged[k] = v
		}
	}
	return connectors.Config{Kind: conn.Kind, Config: merged, Secret: conn.Secret}, nil
}

type pgSourceStore struct {
	db        *pgxpool.Pool
	masterKey string
}

const sourceCols = `id, bucket, object_key, COALESCE(etag, ''), COALESCE(size_bytes, 0)`

func scanSource(row pgx.Row) (registeredSource, error) {
	var s registeredSource
	err := row.Scan(&s.ID, &s.Bucket, &s.Key, &s.ETag, &s.Size)
	return s, err
}

func (p pgSourceStore) SourceByID(ctx context.Context, orgID, id int64) (registeredSource, error) {
	return scanSource(p.db.QueryRow(ctx,
		`SELECT `+sourceCols+` FROM sources WHERE id = $1 AND org_id = $2`, id, orgID))
}

func (p pgSourceStore) SourceByObject(ctx context.Context, orgID int64, bucket, key string) (registeredSource, error) {
	return scanSource(p.db.QueryRow(ctx,
		`SELECT `+sourceCols+` FROM sources WHERE org_id = $1 AND bucket = $2 AND object_key = $3`,
		orgID, bucket, key))
}

func (p pgSourceStore) Connector(ctx context.Context, orgID, id int64) (storedConnector, error) {
	var (
		c        storedConnector
		raw      []byte
		secretID *int64
	)
	if err := p.db.QueryRow(ctx, `
		SELECT connector_kind::text, config_json, secret_id
		FROM connectors WHERE id = $1 AND org_id = $2`, id, orgID,
	).Scan(&c.Kind, &raw, &secretID); err != nil {
		return c, err
	}
	c.Kind = strings.ToLower(c.Kind)
	if err := json.Unmarshal(raw, &c.Config); err != nil {
		return c, fmt.Errorf("connector %d config: %w", id, err)
	}
	if secretID == nil {
		return c, nil
	}
	var ct, nonce []byte
	if err := p.db.QueryRow(ctx,
		`SELECT ciphertext, nonce FROM secrets WHERE id = $1 AND org_id = $2`, *secretID, orgID,
	).Scan(&ct, &nonce); err != nil {
		return c, fmt.Errorf("connector %d secret: %w", id, err)
	}
	plain, err := security.DecryptSecret(p.masterKey, ct, nonce)
	if err != nil {
		return c, fmt.Errorf("decrypt connector %d secret: %w", id, err)
	}
	c.Secret = string(plain)
	return c, nil
}

type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type newSource struct {
	OrgID      int64
	Bucket     string
	Key        string
	Filename   string
	ETag       string
	Size       int64
	Provenance string
}

// registerSource upserts the registry row for an object the platform itself
// discovered or staged for OrgID. A changed ETag/size on the same key updates
// the row, so jobs still reading the old bytes fail with SourceChanged.
func registerSource(ctx context.Context, q rowQuerier, s newSource) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `
		INSERT INTO sources (org_id, bucket, object_key, etag, size_bytes, filename, format, provenance)
		VALUES ($1, $2, $3, NULLIF($4::text, ''), NULLIF($5::bigint, 0), $6, $7, $8)
		ON CONFLICT (org_id, bucket, object_key) DO UPDATE
		  SET etag = COALESCE(EXCLUDED.etag, sources.etag),
		      size_bytes = COALESCE(EXCLUDED.size_bytes, sources.size_bytes)
		RETURNING id`,
		s.OrgID, s.Bucket, s.Key, s.ETag, s.Size, s.Filename, sourceFormat(s.Filename), s.Provenance,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("register source %s/%s: %w", s.Bucket, s.Key, err)
	}
	return id, nil
}

func sourceFormat(filename string) string {
	lower := strings.ToLower(filename)
	if strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz") || strings.HasSuffix(lower, ".zip") {
		return "archive"
	}
	return connectors.FormatFromFilename(filename)
}
