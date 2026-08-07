package batch_test

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/migration-platform/orchestrator/internal/batch"
)

func TestParseManifestValid(t *testing.T) {
	raw := []byte(`{
		"version": 1,
		"batchId": "demo-1",
		"onStageFailure": "continue",
		"stages": [
			{"id": "customers", "file": "customers.csv", "templateKey": "customer-upsert"},
			{"id": "orders", "file": "orders.csv", "templateKey": "order-upsert", "onStageFailure": "stop"}
		]
	}`)
	m, err := batch.ParseManifest(raw)
	require.NoError(t, err)
	require.Equal(t, "continue", m.OnStageFailure)
	require.Equal(t, "stop", m.EffectiveOnFailure(m.Stages[1]))
	require.Equal(t, "continue", m.EffectiveOnFailure(m.Stages[0]))
}

func TestParseManifestRejectsTraversal(t *testing.T) {
	_, err := batch.ParseManifest([]byte(`{
		"version": 1,
		"stages": [{"id": "x", "file": "../etc/passwd", "templateKey": "t"}]
	}`))
	require.Error(t, err)
}

func TestUnpackTarGzRejectsParentPath(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "bad.tar.gz")
	writeTarGz(t, archive, map[string]string{
		"../evil.csv": "x",
	})
	_, err := batch.UnpackArchive(archive, filepath.Join(dir, "out"), batch.DefaultUnpackLimits())
	require.Error(t, err)
}

func TestUnpackZipHappyPath(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "ok.zip")
	writeZip(t, archive, map[string]string{
		"manifest.json": `{"version":1,"stages":[{"id":"a","file":"a.csv","templateKey":"t"}]}`,
		"a.csv":         "id,name\n1,a\n",
	})
	out, err := batch.UnpackArchive(archive, filepath.Join(dir, "out"), batch.DefaultUnpackLimits())
	require.NoError(t, err)
	require.Contains(t, out.Files, "manifest.json")
	require.Contains(t, out.Files, "a.csv")
}

func TestIsArchive(t *testing.T) {
	require.True(t, batch.IsArchive("incoming/pack.tar.gz"))
	require.True(t, batch.IsArchive("x.tgz"))
	require.True(t, batch.IsArchive("x.ZIP"))
	require.False(t, batch.IsArchive("x.csv"))
}

func TestParseManifestDependsOnValid(t *testing.T) {
	m, err := batch.ParseManifest([]byte(`{
		"version": 1,
		"stages": [
			{"id": "a", "file": "a.csv", "templateKey": "t"},
			{"id": "b", "file": "b.csv", "templateKey": "t"},
			{"id": "c", "file": "c.csv", "templateKey": "t", "dependsOn": ["a", "b"]}
		]
	}`))
	require.NoError(t, err)
	require.True(t, m.UsesDependsOn())
	require.Equal(t, []string{"a", "b"}, m.Stages[2].DependsOn)
}

func TestParseManifestNoDependsOnKeepsSequential(t *testing.T) {
	m, err := batch.ParseManifest([]byte(`{
		"version": 1,
		"stages": [
			{"id": "a", "file": "a.csv", "templateKey": "t"},
			{"id": "b", "file": "b.csv", "templateKey": "t"}
		]
	}`))
	require.NoError(t, err)
	require.False(t, m.UsesDependsOn())
}

func TestParseManifestRejectsUnknownDependsOn(t *testing.T) {
	_, err := batch.ParseManifest([]byte(`{
		"version": 1,
		"stages": [
			{"id": "a", "file": "a.csv", "templateKey": "t", "dependsOn": ["missing"]}
		]
	}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown stage")
}

func TestParseManifestRejectsDependsOnCycle(t *testing.T) {
	_, err := batch.ParseManifest([]byte(`{
		"version": 1,
		"stages": [
			{"id": "a", "file": "a.csv", "templateKey": "t", "dependsOn": ["b"]},
			{"id": "b", "file": "b.csv", "templateKey": "t", "dependsOn": ["a"]}
		]
	}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "cycle")
}

func TestParseManifestRejectsSelfDependsOn(t *testing.T) {
	_, err := batch.ParseManifest([]byte(`{
		"version": 1,
		"stages": [
			{"id": "a", "file": "a.csv", "templateKey": "t", "dependsOn": ["a"]}
		]
	}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "itself")
}

func TestReadyStageIDsAndBlocking(t *testing.T) {
	nodes := []batch.StageNode{
		{ID: "a"},
		{ID: "b"},
		{ID: "c", DependsOn: []string{"a", "b"}},
	}
	status := map[string]batch.StageRunStatus{
		"a": batch.StagePending,
		"b": batch.StagePending,
		"c": batch.StagePending,
	}
	require.Equal(t, []string{"a", "b"}, batch.ReadyStageIDs(nodes, status))

	status["a"] = batch.StageSucceeded
	status["b"] = batch.StageFailed
	require.True(t, batch.DepsBlocking(nodes[2], status))
	require.Empty(t, batch.ReadyStageIDs(nodes, status))

	status["b"] = batch.StageSucceeded
	require.Equal(t, []string{"c"}, batch.ReadyStageIDs(nodes, status))
}

func writeTarGz(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	for name, body := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}
		require.NoError(t, tw.WriteHeader(hdr))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}
}

func writeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	zw := zip.NewWriter(f)
	defer zw.Close()
	for name, body := range files {
		w, err := zw.Create(name)
		require.NoError(t, err)
		_, err = w.Write([]byte(body))
		require.NoError(t, err)
	}
}
