package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/updater"
)

func isolatedServerEnvironment(root string) []string {
	result := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(entry), "OMC_") {
			result = append(result, entry)
		}
	}
	return append(result, "OMC_DATABASE_PATH="+filepath.Join(root, "data", "server.db"), "OMC_RUNTIME_DIR="+root, "OMC_LOG_DIR="+filepath.Join(root, "logs"), "OMC_CREDENTIAL_KEY_FILE="+filepath.Join(root, "data", "credentials.key"), "OMC_PLUGIN_DIR="+filepath.Join(root, "plugins"), "OMC_ENV=development", "OMC_SERVER_HOST=127.0.0.1")
}

func buildCompatibilityServer(t *testing.T) string {
	t.Helper()
	name := "ohmycine-server"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	executable := filepath.Join(t.TempDir(), name)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "build", "-o", executable, ".")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build isolated Server: %v\n%s", err, output)
	}
	return executable
}

func runCompatibilityServerUntilHealthy(t *testing.T, executable, root string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable)
	command.Dir = root
	command.Env = append(isolatedServerEnvironment(root), fmt.Sprintf("OMC_SERVER_PORT=%d", port), fmt.Sprintf("OMC_PUBLIC_ORIGIN=http://127.0.0.1:%d", port))
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	client := &http.Client{Timeout: 250 * time.Millisecond}
	healthy := false
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/v1/health", port))
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				healthy = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = command.Process.Kill()
	_ = command.Wait()
	stopped = true
	if !healthy {
		t.Fatalf("isolated startup did not become healthy:\n%s", output.String())
	}
}

func TestCompiledServerCatalogCompatibilityStartup(t *testing.T) {
	executable := buildCompatibilityServer(t)
	t.Run("capability command bypasses config and database", func(t *testing.T) {
		root := t.TempDir()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, executable, updater.CompatibilityCommand)
		command.Dir = root
		command.Env = append(isolatedServerEnvironment(root), "OMC_SERVER_PORT=invalid-must-not-be-read")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("read-only capability command failed: %v\n%s", err, output)
		}
		var capability struct {
			CatalogFormat  int `json:"catalog_format"`
			HelperProtocol int `json:"helper_protocol"`
		}
		if err := json.Unmarshal(output, &capability); err != nil || capability.CatalogFormat != 1 || capability.HelperProtocol != 1 {
			t.Fatalf("invalid capability response: %s err=%v", output, err)
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 0 {
			t.Fatalf("capability command created runtime state: %v err=%v", entries, err)
		}
	})
	t.Run("future floor refuses before database and logging", func(t *testing.T) {
		root := t.TempDir()
		data := filepath.Join(root, "data")
		if err := os.Mkdir(data, 0o700); err != nil {
			t.Fatal(err)
		}
		dbPath := filepath.Join(data, "server.db")
		if err := os.WriteFile(dbPath, []byte("database sentinel: never open"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dbPath+".catalog-format.json", []byte(`{"schema":1,"catalog":2}`), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, executable)
		command.Dir = root
		command.Env = isolatedServerEnvironment(root)
		output, err := command.CombinedOutput()
		if err == nil || !bytes.Contains(output, []byte(updater.CodeFormatIncompatible)) {
			t.Fatalf("future format not safely refused: %v\n%s", err, output)
		}
		payload, _ := os.ReadFile(dbPath)
		if string(payload) != "database sentinel: never open" {
			t.Fatal("preflight changed database")
		}
		for _, path := range []string{filepath.Join(root, "logs"), filepath.Join(data, "credentials.key"), dbPath + "-wal"} {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("preflight touched runtime path: %v", err)
			}
		}
	})
	t.Run("legacy database starts without conversion", func(t *testing.T) {
		root := t.TempDir()
		dbPath := filepath.Join(root, "data", "server.db")
		db, err := database.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.Migrate(db); err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("CREATE TABLE startup_compatibility_sentinel (value TEXT NOT NULL)").Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("INSERT INTO startup_compatibility_sentinel(value) VALUES ('preserved')").Error; err != nil {
			t.Fatal(err)
		}
		sqlDB, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		if err := sqlDB.Close(); err != nil {
			t.Fatal(err)
		}
		runCompatibilityServerUntilHealthy(t, executable, root)
		db, err = database.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		sqlDB, err = db.DB()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = sqlDB.Close() }()
		format, err := database.ReadCatalogFormat(context.Background(), db)
		if err != nil || format != 0 {
			t.Fatalf("legacy startup activated catalog: format=%d err=%v", format, err)
		}
		var value string
		if err := db.Raw("SELECT value FROM startup_compatibility_sentinel").Scan(&value).Error; err != nil || value != "preserved" {
			t.Fatalf("legacy data changed: %q err=%v", value, err)
		}
		if _, err := os.Stat(dbPath + ".catalog-format.json"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("legacy startup raised format floor: %v", err)
		}
	})
	t.Run("fresh install restarts before conversion", func(t *testing.T) {
		root := t.TempDir()
		dbPath := filepath.Join(root, "data", "server.db")
		for attempt := 0; attempt < 2; attempt++ {
			runCompatibilityServerUntilHealthy(t, executable, root)
			guard, err := updater.CheckStartupCompatibility(dbPath, root, executable, nil)
			if err != nil || !guard.CanActivateCatalog() {
				t.Fatalf("clean-origin capability lost on boot %d: %v", attempt, err)
			}
			if _, err := os.Stat(dbPath + ".catalog-origin.json"); err != nil {
				t.Fatalf("successful fresh init did not persist origin: %v", err)
			}
			if _, err := os.Stat(dbPath + ".catalog-format.json"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("fresh startup activated format: %v", err)
			}
		}
		db, err := database.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		sqlDB, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = sqlDB.Close() }()
		format, err := database.ReadCatalogFormat(context.Background(), db)
		if err != nil || format != 0 {
			t.Fatalf("fresh startup format=%d err=%v", format, err)
		}
	})
}

func TestDatabaseFormatValidationRefusesMissingProtection(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "server.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlDB.Close() }()
	guard, err := updater.CheckStartupCompatibility(dbPath, root, os.Args[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCatalogDatabaseCompatibility(context.Background(), db, guard); err != nil {
		t.Fatalf("pre-migration legacy rejected: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := validateCatalogDatabaseCompatibility(context.Background(), db, guard); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("UPDATE catalog_format_floor SET format=1 WHERE id=1").Error; err != nil {
		t.Fatal(err)
	}
	if err := validateCatalogDatabaseCompatibility(context.Background(), db, guard); updater.ErrorCode(err) != updater.CodeFormatIncompatible {
		t.Fatalf("activated database without protection accepted: %v", err)
	}
}
