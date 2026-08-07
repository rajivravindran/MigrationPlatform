package batch

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// UnpackLimits hard-caps archive expansion to mitigate zip bombs / path escapes.
type UnpackLimits struct {
	MaxCompressedBytes   int64
	MaxUncompressedBytes int64
	MaxEntries           int
}

// DefaultUnpackLimits returns production defaults (overridable via env in activities).
func DefaultUnpackLimits() UnpackLimits {
	return UnpackLimits{
		MaxCompressedBytes:   256 << 20, // 256 MiB
		MaxUncompressedBytes: 1 << 30,   // 1 GiB
		MaxEntries:           1000,
	}
}

// UnpackResult is the safe extract of a batch archive.
type UnpackResult struct {
	RootDir string
	Files   map[string]string // relative path -> absolute path on disk
}

// UnpackArchive extracts archivePath into destDir with security caps.
// Supports .tar.gz / .tgz / .zip. Rejects symlinks, absolute paths, and `..`.
func UnpackArchive(archivePath, destDir string, lim UnpackLimits) (*UnpackResult, error) {
	if lim.MaxCompressedBytes <= 0 {
		lim = DefaultUnpackLimits()
	}
	st, err := os.Stat(archivePath)
	if err != nil {
		return nil, err
	}
	if st.Size() > lim.MaxCompressedBytes {
		return nil, fmt.Errorf("compressed archive size %d exceeds limit %d", st.Size(), lim.MaxCompressedBytes)
	}
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return nil, err
	}

	lower := strings.ToLower(archivePath)
	switch {
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return unpackTarGz(archivePath, destDir, lim)
	case strings.HasSuffix(lower, ".zip"):
		return unpackZip(archivePath, destDir, lim)
	default:
		return nil, fmt.Errorf("unsupported archive type: %s", filepath.Base(archivePath))
	}
}

func unpackTarGz(archivePath, destDir string, lim UnpackLimits) (*UnpackResult, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	out := &UnpackResult{RootDir: destDir, Files: map[string]string{}}
	var total int64
	entries := 0

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar: %w", err)
		}
		entries++
		if entries > lim.MaxEntries {
			return nil, fmt.Errorf("archive entry count exceeds limit %d", lim.MaxEntries)
		}
		name := filepath.ToSlash(hdr.Name)
		name = strings.TrimPrefix(name, "./")
		if name == "" || strings.HasSuffix(name, "/") {
			continue
		}
		if err := ValidateRelPath(name); err != nil {
			return nil, fmt.Errorf("unsafe tar entry %q: %w", hdr.Name, err)
		}
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			// ok
		case tar.TypeDir:
			continue
		case tar.TypeSymlink, tar.TypeLink:
			return nil, fmt.Errorf("symlinks/hardlinks not allowed: %q", hdr.Name)
		default:
			return nil, fmt.Errorf("unsupported tar entry type %c for %q", hdr.Typeflag, hdr.Name)
		}
		if hdr.Size < 0 {
			return nil, fmt.Errorf("invalid size for %q", hdr.Name)
		}
		if total+hdr.Size > lim.MaxUncompressedBytes {
			return nil, fmt.Errorf("uncompressed size would exceed limit %d", lim.MaxUncompressedBytes)
		}
		abs, err := safeJoin(destDir, name)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
			return nil, err
		}
		n, err := writeLimitedFile(abs, tr, hdr.Size, lim.MaxUncompressedBytes-total)
		if err != nil {
			return nil, err
		}
		total += n
		out.Files[name] = abs
	}
	return out, nil
}

func unpackZip(archivePath, destDir string, lim UnpackLimits) (*UnpackResult, error) {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	if len(r.File) > lim.MaxEntries {
		return nil, fmt.Errorf("archive entry count exceeds limit %d", lim.MaxEntries)
	}
	out := &UnpackResult{RootDir: destDir, Files: map[string]string{}}
	var total int64

	for _, zf := range r.File {
		name := filepath.ToSlash(zf.Name)
		name = strings.TrimPrefix(name, "./")
		if name == "" || strings.HasSuffix(name, "/") {
			continue
		}
		if err := ValidateRelPath(name); err != nil {
			return nil, fmt.Errorf("unsafe zip entry %q: %w", zf.Name, err)
		}
		if zf.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("symlinks not allowed: %q", zf.Name)
		}
		if zf.FileInfo().IsDir() {
			continue
		}
		declared := int64(zf.UncompressedSize64)
		if declared < 0 {
			return nil, fmt.Errorf("invalid size for %q", zf.Name)
		}
		if total+declared > lim.MaxUncompressedBytes {
			return nil, fmt.Errorf("uncompressed size would exceed limit %d", lim.MaxUncompressedBytes)
		}
		rc, err := zf.Open()
		if err != nil {
			return nil, err
		}
		abs, err := safeJoin(destDir, name)
		if err != nil {
			_ = rc.Close()
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
			_ = rc.Close()
			return nil, err
		}
		n, err := writeLimitedFile(abs, rc, declared, lim.MaxUncompressedBytes-total)
		_ = rc.Close()
		if err != nil {
			return nil, err
		}
		total += n
		out.Files[name] = abs
	}
	return out, nil
}

func safeJoin(root, rel string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	abs := filepath.Join(root, clean)
	relOut, err := filepath.Rel(root, abs)
	if err != nil || strings.HasPrefix(relOut, "..") {
		return "", fmt.Errorf("path escapes extract root: %q", rel)
	}
	return abs, nil
}

func writeLimitedFile(abs string, r io.Reader, declared, remaining int64) (int64, error) {
	limit := remaining
	if declared > 0 && declared < limit {
		limit = declared
	}
	// +1 so we detect overshoot past declared size.
	limited := io.LimitReader(r, limit+1)
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n, err := io.Copy(f, limited)
	if err != nil {
		return n, err
	}
	if n > remaining || (declared > 0 && n > declared) {
		_ = os.Remove(abs)
		return n, fmt.Errorf("entry exceeds size budget")
	}
	return n, nil
}
