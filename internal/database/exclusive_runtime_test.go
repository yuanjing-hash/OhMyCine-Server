package database

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestExclusiveRuntimeBindsExactDatabaseAndLifetime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	r, err := AcquireExclusiveRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("lock created database before compatibility guard")
	}
	if other, err := AcquireExclusiveRuntime(path); err == nil {
		_ = other.Close()
		t.Fatal("second runtime acquired live lock")
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	defer func() { _ = pool.Close() }()
	bound, err := r.Bind(db)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ExclusiveRuntimeID(bound)
	if err != nil || id == "" {
		t.Fatalf("no runtime capability: %v", err)
	}
	if err := bound.Transaction(func(tx *gorm.DB) error {
		got, err := ExclusiveRuntimeID(tx)
		if got != id {
			t.Error("transaction lost runtime capability")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Proof queries must not leak PRAGMA or any previous GORM statement into
	// the next service query on the shared root.
	if err := bound.Exec("CREATE TABLE runtime_probe (id INTEGER)").Error; err != nil {
		t.Fatal(err)
	}
	if err := bound.Exec("INSERT INTO runtime_probe VALUES (1)").Error; err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := bound.Table("runtime_probe").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("query state leaked: count=%d err=%v", count, err)
	}
	otherDB, err := Open(filepath.Join(t.TempDir(), "other.db"))
	if err != nil {
		t.Fatal(err)
	}
	otherPool, _ := otherDB.DB()
	defer func() { _ = otherPool.Close() }()
	if _, err := r.Bind(otherDB); !errors.Is(err, ErrExclusiveRuntime) {
		t.Fatal("foreign DB accepted")
	}
	if _, err := ExclusiveRuntimeID(otherDB.Set(exclusiveRuntimeSetting, r)); !errors.Is(err, ErrExclusiveRuntime) {
		t.Fatal("copied capability accepted foreign DB")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ExclusiveRuntimeID(bound); !errors.Is(err, ErrExclusiveRuntime) {
		t.Fatal("released lock retained capability")
	}
	if _, err := os.Stat(path + ".runtime.lock"); err != nil {
		t.Fatal("lock inode removed")
	}
	next, err := AcquireExclusiveRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = next.Close() }()
	if next.id == id {
		t.Fatal("restart reused runtime identity")
	}
}

func TestExclusiveRuntimeRejectsHardlinksAndInvalidPaths(t *testing.T) {
	for _, path := range []string{"", ":memory:", "file:test.db", "test.db?mode=memory"} {
		if r, err := AcquireExclusiveRuntime(path); err == nil {
			_ = r.Close()
			t.Fatalf("accepted invalid path %q", path)
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.db")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias.db")
	if err := os.Link(path, alias); err != nil {
		t.Skipf("hardlink unavailable: %v", err)
	}
	for _, target := range []string{path, alias} {
		if r, err := AcquireExclusiveRuntime(target); err == nil {
			_ = r.Close()
			t.Fatal("hardlinked DB bypasses sidecar lock identity")
		}
	}
}

func TestExclusiveRuntimeProcessHelper(t *testing.T) {
	path := os.Getenv("OMC_TEST_EXCLUSIVE_RUNTIME_PATH")
	if path == "" {
		return
	}
	r, err := AcquireExclusiveRuntime(path)
	if err != nil {
		os.Exit(2)
	}
	defer func() { _ = r.Close() }()
	_, _ = os.Stdout.WriteString("runtime-lock-ready\n")
	// The parent kills this exact test child to prove OS-release-on-crash.
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

func TestExclusiveRuntimeSurvivesProcessContentionAndCrash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExclusiveRuntimeProcessHelper$")
	cmd.Env = append(os.Environ(), "OMC_TEST_EXCLUSIVE_RUNTIME_PATH="+path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || line != "runtime-lock-ready\n" {
		t.Fatalf("child not ready: %q %v", line, err)
	}
	if r, err := AcquireExclusiveRuntime(path); err == nil {
		_ = r.Close()
		t.Fatal("live child did not exclude second process")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("crashed child unexpectedly succeeded")
	}
	waited = true
	r, err := AcquireExclusiveRuntime(path)
	if err != nil {
		t.Fatal("OS did not release crashed process lock", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}
