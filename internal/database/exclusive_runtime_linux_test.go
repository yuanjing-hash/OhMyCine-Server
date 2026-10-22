//go:build linux

package database

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestExclusiveRuntimePreservesSQLitePOSIXLocks(t *testing.T) {
	const probeEnvironment = "OMC_TEST_SQLITE_POSIX_LOCK_PATH"
	if path := os.Getenv(probeEnvironment); path != "" {
		// Probe in a different process: POSIX locks belong to a process, so a
		// same-process probe would never conflict with SQLite's own locks.
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 0x40000000, Len: 512}
		err = unix.FcntlFlock(f.Fd(), unix.F_SETLK, &lock)
		if !errors.Is(err, unix.EACCES) && !errors.Is(err, unix.EAGAIN) {
			t.Fatalf("SQLite main-file lock was lost: competing POSIX lock err=%v", err)
		}
		return
	}

	for _, acquireAfterOpen := range []bool{false, true} {
		name := "bind_after_open"
		if acquireAfterOpen {
			name = "acquire_after_open"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "catalog.db")
			var capability *ExclusiveRuntime
			acquire := func() {
				t.Helper()
				var err error
				capability, err = AcquireExclusiveRuntime(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = capability.Close() })
			}
			if !acquireAfterOpen {
				acquire()
			}
			db, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			pool, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			pool.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = pool.Close() })
			if err := db.Exec("CREATE TABLE omc_lock_probe (id INTEGER)").Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Exec("INSERT INTO omc_lock_probe VALUES (1)").Error; err != nil {
				t.Fatal(err)
			}
			assertSQLiteLock := func(stage string) {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExclusiveRuntimePreservesSQLitePOSIXLocks$")
				cmd.Env = append(os.Environ(), probeEnvironment+"="+path)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("%s lost actual SQLite exclusion: %v\n%s", stage, err, out)
				}
			}
			assertSQLiteLock("initial SQLite open")
			if acquireAfterOpen {
				acquire()
				assertSQLiteLock("AcquireExclusiveRuntime")
			}
			bound, err := capability.Bind(db)
			if err != nil {
				t.Fatal(err)
			}
			assertSQLiteLock("Bind")
			if id, err := ExclusiveRuntimeID(bound); err != nil || id == "" {
				t.Fatalf("runtime identity: id=%q err=%v", id, err)
			}
			assertSQLiteLock("ExclusiveRuntimeID")
			if err := capability.Close(); err != nil {
				t.Fatal(err)
			}
			assertSQLiteLock("runtime lock close")
		})
	}
}
