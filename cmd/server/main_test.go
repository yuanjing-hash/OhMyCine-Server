package main

import (
	"path/filepath"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/config"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/updater"
)

func TestRunUpdateHelperRejectsMalformedPrivateInvocation(t *testing.T) {
	if handled, code := runUpdateHelper([]string{"server"}); handled || code != 0 {
		t.Fatalf("ordinary start handled=%v code=%d", handled, code)
	}
	if handled, code := runUpdateHelper([]string{"server", updater.HelperFlag}); !handled || code != 2 {
		t.Fatalf("malformed helper handled=%v code=%d", handled, code)
	}
}

func TestResolveUpdateRuntimeDirectoryPrefersExplicitRuntime(t *testing.T) {
	runtimeDirectory := filepath.Join(t.TempDir(), "runtime")
	t.Setenv("OMC_RUNTIME_DIR", runtimeDirectory)
	resolved, err := resolveUpdateRuntimeDirectory(config.Config{DatabasePath: filepath.Join(t.TempDir(), "other", "data", "server.db")})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.Abs(runtimeDirectory)
	if resolved != filepath.Clean(want) {
		t.Fatalf("resolved=%q want=%q", resolved, want)
	}
}

func TestResolveUpdateRuntimeDirectoryFromDatabaseDataDirectory(t *testing.T) {
	t.Setenv("OMC_RUNTIME_DIR", "")
	runtimeDirectory := t.TempDir()
	resolved, err := resolveUpdateRuntimeDirectory(config.Config{DatabasePath: filepath.Join(runtimeDirectory, "data", "server.db")})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.Abs(runtimeDirectory)
	if resolved != filepath.Clean(want) {
		t.Fatalf("resolved=%q want=%q", resolved, want)
	}
}
