package updater

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func releaseArchive(t *testing.T, names PlatformAssets, payload []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if strings.HasSuffix(names.Archive, ".zip") {
		writer := zip.NewWriter(&buffer)
		entry, err := writer.Create(names.TopLevel + "/" + names.Binary)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		return buffer.Bytes()
	}
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: names.TopLevel + "/" + names.Binary, Mode: 0o755, Typeflag: tar.TypeReg, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestPrepareCreatesIndependentHelperAndPrivateBoundPlan(t *testing.T) {
	names, err := AssetNames("2.0.0", runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skipf("self-update is not supported on test platform: %v", err)
	}
	candidatePayload := []byte("candidate-server-binary")
	archive := releaseArchive(t, names, candidatePayload)
	digest := sha256.Sum256(archive)
	base := "https://github.com/yuanjing-hash/OhMyCine-Server/releases/download/server-v2.0.0/"
	release := Release{TagName: "server-v2.0.0", Prerelease: true, Assets: []Asset{
		{Name: names.Archive, DownloadURL: base + names.Archive, Size: int64(len(archive))},
		{Name: names.Checksum, DownloadURL: base + names.Checksum, Size: 128},
	}}
	selected, err := ValidateRelease(release, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	manifest := []byte(fmt.Sprintf("%x  %s\n", digest, names.Archive))
	client := NewGitHubClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.HasSuffix(request.URL.Path, names.Checksum) {
			return response(request, http.StatusOK, manifest), nil
		}
		return response(request, http.StatusOK, archive), nil
	})})
	runtimeRoot := t.TempDir()
	store, err := NewStore(runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(runtimeRoot, "bin", names.Binary)
	if err := os.MkdirAll(filepath.Dir(current), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(current, []byte("old-server-binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	prepared, err := client.Prepare(context.Background(), selected, store, PrepareRequest{CurrentExecutable: current, ParentPID: os.Getpid(), OriginalArgs: []string{"--config", "server.json"}, HealthURL: "http://127.0.0.1:3000/api/v1/health"})
	if err != nil {
		t.Fatal(err)
	}
	helperPayload, err := os.ReadFile(prepared.HelperExecutable)
	if err != nil || !bytes.Equal(helperPayload, candidatePayload) {
		t.Fatalf("helper is not an independent candidate copy: %q err=%v", helperPayload, err)
	}
	plan, err := LoadPlanFile(prepared.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	if plan.OperationID != prepared.OperationID || plan.CurrentSHA256 == "" || plan.TargetVersion != "2.0.0" || len(plan.OriginalArgs) != 2 {
		t.Fatalf("unexpected prepared plan: %+v", plan)
	}
	if plan.Candidate == prepared.HelperExecutable {
		t.Fatal("running helper must not be the candidate that will be renamed")
	}
}
