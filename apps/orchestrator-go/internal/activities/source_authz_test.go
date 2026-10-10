package activities

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"

	"github.com/migration-platform/orchestrator/internal/connectors"
)

type fakeSourceStore struct {
	sources    map[int64]registeredSource // keyed by id; ownerOf gives the org
	ownerOf    map[int64]int64
	connectors map[int64]storedConnector
	connOwner  map[int64]int64
}

func (f fakeSourceStore) SourceByID(_ context.Context, orgID, id int64) (registeredSource, error) {
	if s, ok := f.sources[id]; ok && f.ownerOf[id] == orgID {
		return s, nil
	}
	return registeredSource{}, pgx.ErrNoRows
}

func (f fakeSourceStore) SourceByObject(_ context.Context, orgID int64, bucket, key string) (registeredSource, error) {
	for id, s := range f.sources {
		if f.ownerOf[id] == orgID && s.Bucket == bucket && s.Key == key {
			return s, nil
		}
	}
	return registeredSource{}, pgx.ErrNoRows
}

func (f fakeSourceStore) Connector(_ context.Context, orgID, id int64) (storedConnector, error) {
	if c, ok := f.connectors[id]; ok && f.connOwner[id] == orgID {
		return c, nil
	}
	return storedConnector{}, pgx.ErrNoRows
}

const orgA, orgB = int64(1), int64(2)

func newTestAuthorizer(stat statFunc) *sourceAuthorizer {
	store := fakeSourceStore{
		sources: map[int64]registeredSource{
			10: {ID: 10, Bucket: "migration", Key: "uploads/1/ab/abc", ETag: "e1", Size: 120},
		},
		ownerOf: map[int64]int64{10: orgA},
		connectors: map[int64]storedConnector{
			3: {Kind: "salesforce", Secret: "stored-secret", Config: map[string]any{
				"tokenUrl": "https://login.example.test/token", "instanceUrl": "https://sf.example.test", "clientId": "cid",
			}},
			4: {Kind: "watched_sftp"},
		},
		connOwner: map[int64]int64{3: orgA, 4: orgA},
	}
	if stat == nil {
		stat = func(context.Context, string, string) (connectors.ObjectInfo, error) {
			return connectors.ObjectInfo{ETag: "e1", Size: 120}, nil
		}
	}
	return &sourceAuthorizer{store: store, stat: stat}
}

func objectInput(org, sourceID int64) IngestInput {
	return IngestInput{
		OrgID: org, SourceID: sourceID, ConnectorKind: "csv",
		ConnectorConfig: map[string]any{"s3_bucket": "migration", "s3_key": "uploads/1/ab/abc", "header": true},
	}
}

func requireErrType(t *testing.T, err error, want string) {
	t.Helper()
	var appErr *temporal.ApplicationError
	require.True(t, errors.As(err, &appErr), "want ApplicationError, got %v", err)
	require.Equal(t, want, appErr.Type())
	require.True(t, appErr.NonRetryable())
}

func TestAuthorizeRegisteredObject(t *testing.T) {
	a := newTestAuthorizer(nil)
	for _, in := range []IngestInput{objectInput(orgA, 10), objectInput(orgA, 0)} {
		cfg, err := a.authorize(context.Background(), in)
		require.NoError(t, err)
		require.Equal(t, "csv", cfg.Kind)
		require.Equal(t, "uploads/1/ab/abc", cfg.Config["s3_key"])
	}
}

func TestAuthorizeRefusesOtherOrgsObject(t *testing.T) {
	a := newTestAuthorizer(nil)
	_, err := a.authorize(context.Background(), objectInput(orgB, 10))
	requireErrType(t, err, ErrTypeSourceNotAuthorized)
	_, err = a.authorize(context.Background(), objectInput(orgB, 0))
	requireErrType(t, err, ErrTypeSourceNotAuthorized)
}

func TestAuthorizeRefusesSourceIDThatNamesDifferentObject(t *testing.T) {
	a := newTestAuthorizer(nil)
	in := objectInput(orgA, 10)
	in.ConnectorConfig["s3_key"] = "uploads/2/cd/other"
	_, err := a.authorize(context.Background(), in)
	requireErrType(t, err, ErrTypeSourceNotAuthorized)
}

func TestAuthorizeDetectsChangedObject(t *testing.T) {
	for name, info := range map[string]connectors.ObjectInfo{
		"etag": {ETag: "e2", Size: 120},
		"size": {ETag: "e1", Size: 121},
	} {
		t.Run(name, func(t *testing.T) {
			a := newTestAuthorizer(func(context.Context, string, string) (connectors.ObjectInfo, error) { return info, nil })
			_, err := a.authorize(context.Background(), objectInput(orgA, 10))
			requireErrType(t, err, ErrTypeSourceChanged)
		})
	}
}

func TestAuthorizeStatFailureIsRetryable(t *testing.T) {
	a := newTestAuthorizer(func(context.Context, string, string) (connectors.ObjectInfo, error) {
		return connectors.ObjectInfo{}, errors.New("object store unavailable")
	})
	_, err := a.authorize(context.Background(), objectInput(orgA, 10))
	require.Error(t, err)
	var appErr *temporal.ApplicationError
	require.False(t, errors.As(err, &appErr), "transient errors must stay retryable")
}

func TestAuthorizeRefusesPathsURLsAndInlineSecrets(t *testing.T) {
	a := newTestAuthorizer(nil)
	cases := map[string]IngestInput{
		"path":           {OrgID: orgA, ConnectorKind: "csv", ConnectorConfig: map[string]any{"path": "/etc/passwd"}},
		"url":            {OrgID: orgA, ConnectorKind: "csv", ConnectorConfig: map[string]any{"url": "http://169.254.169.254/"}},
		"object+url":     func() IngestInput { in := objectInput(orgA, 10); in.ConnectorConfig["url"] = "http://x"; return in }(),
		"inline secret":  {OrgID: orgA, ConnectorKind: "csv", ConnectorSecret: "tok", ConnectorConfig: map[string]any{}},
		"salesforce raw": {OrgID: orgA, ConnectorKind: "salesforce", ConnectorConfig: map[string]any{"instanceUrl": "http://127.0.0.1"}},
		"watched kind":   {OrgID: orgA, ConnectorKind: "watched_prefix", ConnectorConfig: map[string]any{"objects": []any{"http://x"}}},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := a.authorize(context.Background(), in)
			requireErrType(t, err, ErrTypeSourceNotAuthorized)
		})
	}
}

func TestAuthorizeLocalPathsWhenExplicitlyAllowed(t *testing.T) {
	a := newTestAuthorizer(nil)
	a.allowLocalPaths = true
	cfg, err := a.authorize(context.Background(), IngestInput{OrgID: orgA, ConnectorKind: "csv", ConnectorConfig: map[string]any{"path": "/data/x.csv"}})
	require.NoError(t, err)
	require.Equal(t, "/data/x.csv", cfg.Config["path"])
}

func TestAuthorizeConnectorLoadsStoredConfigAndSecret(t *testing.T) {
	a := newTestAuthorizer(nil)
	cfg, err := a.authorize(context.Background(), IngestInput{
		OrgID: orgA, ConnectorID: 3, ConnectorKind: "salesforce",
		ConnectorConfig: map[string]any{
			"soql":        "SELECT Id FROM Account",
			"instanceUrl": "http://127.0.0.1:9999", // must be ignored
			"tokenUrl":    "http://evil.example/token",
		},
	})
	require.NoError(t, err)
	require.Equal(t, "salesforce", cfg.Kind)
	require.Equal(t, "stored-secret", cfg.Secret)
	require.Equal(t, "https://sf.example.test", cfg.Config["instanceUrl"])
	require.Equal(t, "https://login.example.test/token", cfg.Config["tokenUrl"])
	require.Equal(t, "SELECT Id FROM Account", cfg.Config["soql"])
}

func TestAuthorizeConnectorIsOrgScopedAndKindChecked(t *testing.T) {
	a := newTestAuthorizer(nil)
	_, err := a.authorize(context.Background(), IngestInput{OrgID: orgB, ConnectorID: 3, ConnectorKind: "salesforce"})
	requireErrType(t, err, ErrTypeSourceNotAuthorized)
	_, err = a.authorize(context.Background(), IngestInput{OrgID: orgA, ConnectorID: 4, ConnectorKind: "salesforce"})
	requireErrType(t, err, ErrTypeSourceNotAuthorized)
}

func TestCheckOrgLocation(t *testing.T) {
	a := newTestAuthorizer(nil)
	a.allowedBuckets = map[string]bool{"migration": true, "inbound-acme": true}

	for _, ok := range []struct{ bucket, key string }{
		{"migration", "orgs/1/incoming/"},
		{"migration", "orgs/1/incoming/a.csv"},
		{"inbound-acme", "/orgs/1/drop/x.tgz"},
		{"migration", connectors.StagingObjectKey("orgs/1/sftp-landing/", 5, "in/a.csv")},
	} {
		require.NoError(t, a.checkOrgLocation(orgA, ok.bucket, ok.key), ok.key)
	}

	for name, bad := range map[string]struct{ bucket, key string }{
		"other org prefix":    {"migration", "orgs/2/incoming/"},
		"org id prefix match": {"migration", "orgs/12/incoming/"},
		"platform uploads":    {"migration", "uploads/2/ab/abc"},
		"platform batches":    {"migration", "batches/7/stages/s1/a.csv"},
		"whole bucket":        {"migration", ""},
		"unlisted bucket":     {"license-backups", "orgs/1/x"},
		"dotdot segment":      {"migration", "orgs/1/../2/x"},
		"backslash":           {"migration", "orgs/1\\..\\2"},
		"sftp name traversal": {"migration", connectors.StagingObjectKey("orgs/1/sftp-landing/", 5, "../../../../uploads/2/ab/abc")},
	} {
		t.Run(name, func(t *testing.T) {
			requireErrType(t, a.checkOrgLocation(orgA, bad.bucket, bad.key), ErrTypeSourceNotAuthorized)
		})
	}
	requireErrType(t, a.checkOrgLocation(0, "migration", "orgs/0/x"), ErrTypeSourceNotAuthorized)
}

func TestSourceFormat(t *testing.T) {
	require.Equal(t, "archive", sourceFormat("pkg.TAR.GZ"))
	require.Equal(t, "json", sourceFormat("a.ndjson"))
	require.Equal(t, "csv", sourceFormat("noext"))
}
