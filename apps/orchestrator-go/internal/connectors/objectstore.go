package connectors

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var s3Client *minio.Client

// InitObjectStore wires a MinIO/S3-compatible client from MINIO_* env vars.
// Upload jobs pass source_ref.type=minio; openSource reads objects via this client.
func InitObjectStore() error {
	endpoint := strings.TrimSpace(os.Getenv("MINIO_ENDPOINT"))
	if endpoint == "" {
		return nil
	}
	accessKey := os.Getenv("MINIO_ACCESS_KEY")
	secretKey := os.Getenv("MINIO_SECRET_KEY")
	if accessKey == "" || secretKey == "" {
		return errors.New("MINIO_ACCESS_KEY and MINIO_SECRET_KEY are required when MINIO_ENDPOINT is set")
	}

	host := endpoint
	secure := false
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		host = u.Host
		secure = u.Scheme == "https"
	}

	client, err := minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: secure,
	})
	if err != nil {
		return err
	}
	s3Client = client
	return nil
}

func openObjectStore(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	if s3Client == nil {
		return nil, errors.New("object store not configured (set MINIO_ENDPOINT on the orchestrator)")
	}
	obj, err := s3Client.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	return obj, nil
}

// ObjectInfo is a listed object under a watched prefix.
type ObjectInfo struct {
	Key          string    `json:"key"`
	ETag         string    `json:"etag"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"lastModified"`
}

// ListPrefixObjects lists objects under bucket/prefix, filters by glob (matched
// against the basename), and sorts by "lexical" (key) or "mtime".
func ListPrefixObjects(ctx context.Context, bucket, prefix, globPattern, sortBy string) ([]ObjectInfo, error) {
	if s3Client == nil {
		return nil, errors.New("object store not configured (set MINIO_ENDPOINT on the orchestrator)")
	}
	if bucket == "" {
		return nil, errors.New("bucket is required")
	}
	if globPattern == "" {
		globPattern = "*"
	}
	if sortBy == "" {
		sortBy = "lexical"
	}

	opts := minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}
	var out []ObjectInfo
	for obj := range s3Client.ListObjects(ctx, bucket, opts) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		if strings.HasSuffix(obj.Key, "/") {
			continue
		}
		base := path.Base(obj.Key)
		ok, err := path.Match(globPattern, base)
		if err != nil {
			return nil, err
		}
		if !ok {
			// Also try matching against the full key for patterns like "incoming/*.csv".
			ok, err = path.Match(globPattern, obj.Key)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		out = append(out, ObjectInfo{
			Key:          obj.Key,
			ETag:         strings.Trim(obj.ETag, "\""),
			Size:         obj.Size,
			LastModified: obj.LastModified,
		})
	}

	switch sortBy {
	case "mtime":
		sort.Slice(out, func(i, j int) bool {
			if out[i].LastModified.Equal(out[j].LastModified) {
				return out[i].Key < out[j].Key
			}
			return out[i].LastModified.Before(out[j].LastModified)
		})
	default: // lexical
		sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	}
	return out, nil
}

// FormatFromFilename maps an uploaded filename to the ingest connector kind.
func FormatFromFilename(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".csv"), strings.HasSuffix(lower, ".tsv"):
		return "csv"
	case strings.HasSuffix(lower, ".json"), strings.HasSuffix(lower, ".ndjson"), strings.HasSuffix(lower, ".jsonl"):
		return "json"
	case strings.HasSuffix(lower, ".xml"):
		return "xml"
	default:
		return "csv"
	}
}

// StatObject returns size (and presence) for a MinIO/S3 object.
func StatObject(ctx context.Context, bucket, key string) (ObjectInfo, error) {
	if s3Client == nil {
		return ObjectInfo{}, errors.New("object store not configured (set MINIO_ENDPOINT on the orchestrator)")
	}
	info, err := s3Client.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{
		Key:          key,
		ETag:         strings.Trim(info.ETag, "\""),
		Size:         info.Size,
		LastModified: info.LastModified,
	}, nil
}

// DownloadObject writes bucket/key to destPath, enforcing maxBytes (0 = no cap).
func DownloadObject(ctx context.Context, bucket, key, destPath string, maxBytes int64) (int64, error) {
	rc, err := openObjectStore(ctx, bucket, key)
	if err != nil {
		return 0, err
	}
	defer rc.Close()

	if err := os.MkdirAll(path.Dir(destPath), 0o750); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var r io.Reader = rc
	if maxBytes > 0 {
		r = io.LimitReader(rc, maxBytes+1)
	}
	n, err := io.Copy(f, r)
	if err != nil {
		return n, err
	}
	if maxBytes > 0 && n > maxBytes {
		_ = os.Remove(destPath)
		return n, fmt.Errorf("object size exceeds limit %d", maxBytes)
	}
	return n, nil
}

// PutObject uploads a local file to bucket/key.
func PutObject(ctx context.Context, bucket, key, filePath, contentType string) error {
	if s3Client == nil {
		return errors.New("object store not configured (set MINIO_ENDPOINT on the orchestrator)")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err := s3Client.FPutObject(ctx, bucket, key, filePath, minio.PutObjectOptions{
		ContentType: contentType,
	})
	return err
}

// CopyObject server-side copies an object within the same bucket.
func CopyObject(ctx context.Context, bucket, srcKey, dstKey string) error {
	if s3Client == nil {
		return errors.New("object store not configured (set MINIO_ENDPOINT on the orchestrator)")
	}
	_, err := s3Client.CopyObject(ctx,
		minio.CopyDestOptions{Bucket: bucket, Object: dstKey},
		minio.CopySrcOptions{Bucket: bucket, Object: srcKey},
	)
	return err
}
