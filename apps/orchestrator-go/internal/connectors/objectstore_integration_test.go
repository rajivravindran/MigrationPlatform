//go:build integration

package connectors

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
)

// F2/F3 against a real S3-compatible server (MP_TEST_S3_ENDPOINT, e.g. a
// moto or MinIO instance): a read bound to a version is refused once the
// object is replaced, and the stream's ETag is what PutObject reported.
func TestObjectReadsAreBoundToTheRegisteredVersion(t *testing.T) {
	endpoint := os.Getenv("MP_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("MP_TEST_S3_ENDPOINT not set")
	}
	t.Setenv("MINIO_ENDPOINT", endpoint)
	t.Setenv("MINIO_ACCESS_KEY", "test")
	t.Setenv("MINIO_SECRET_KEY", "test")
	if err := InitObjectStore(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	bucket := "mp-f2-" + strings.ToLower(t.Name()[len("TestObjectReads"):])
	bucket = strings.ReplaceAll(bucket, "_", "-")
	if err := s3Client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	v1, err := PutObject(ctx, bucket, "orgs/1/a.csv", write("a1.csv", "id\n1\n"), "text/csv")
	if err != nil {
		t.Fatal(err)
	}
	if v1.ETag == "" {
		t.Fatal("PutObject must report the stored version")
	}
	rc, err := openObjectStore(ctx, bucket, "orgs/1/a.csv", v1.ETag)
	if err != nil {
		t.Fatalf("read bound to the current version: %v", err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(body) != "id\n1\n" {
		t.Fatalf("body %q", body)
	}

	// A producer replaces the object between validation and read.
	v2, err := PutObject(ctx, bucket, "orgs/1/a.csv", write("a2.csv", "id\n2\n"), "text/csv")
	if err != nil {
		t.Fatal(err)
	}
	if v2.ETag == v1.ETag {
		t.Fatal("different content must have a different version")
	}
	if _, err := openObjectStore(ctx, bucket, "orgs/1/a.csv", v1.ETag); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("read bound to the old version must fail closed, got %v", err)
	}
	if _, err := DownloadObject(ctx, bucket, "orgs/1/a.csv", filepath.Join(dir, "dl.csv"), 0, v1.ETag); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("download bound to the old version must fail closed, got %v", err)
	}
	n, err := DownloadObject(ctx, bucket, "orgs/1/a.csv", filepath.Join(dir, "dl.csv"), 0, v2.ETag)
	if err != nil || n != int64(len("id\n2\n")) {
		t.Fatalf("download of the current version: n=%d err=%v", n, err)
	}
	// Unbound reads (legacy callers) still work.
	if rc, err := openObjectStore(ctx, bucket, "orgs/1/a.csv", ""); err != nil {
		t.Fatal(err)
	} else {
		_ = rc.Close()
	}
}
