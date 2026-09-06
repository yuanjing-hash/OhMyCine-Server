package updater

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The test executable exercises the same early, read-only capability command
// as cmd/server, without loading configuration, opening a DB or starting HTTP.
func TestMain(m *testing.M) {
	if handled, err := RunCompatibilityCommand(os.Args, os.Stdout); handled {
		if err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func startupFixture(t *testing.T) (string, string, string) {
	t.Helper()
	runtime := t.TempDir()
	_, err := NewStore(runtime)
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(runtime, "server.db")
	exe := filepath.Join(runtime, "server.exe")
	for path, payload := range map[string]string{db: "database sentinel", exe: "candidate binary"} {
		if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return db, runtime, exe
}

func writeReceipt(t *testing.T, db, runtime, exe string, completed bool) compatibilityReceipt {
	t.Helper()
	digest, err := hashFile(exe, MaxCandidateBytes)
	if err != nil {
		t.Fatal(err)
	}
	id, err := newOperationID()
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := newOperationID()
	if err != nil {
		t.Fatal(err)
	}
	receipt := compatibilityReceipt{Schema: 1, OperationID: id, Nonce: nonce, CandidateSHA256: digest, DatabasePath: db, Completed: completed}
	if err := atomicWriteJSON(compatibilityPath(runtime), receipt, 0o600); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestOldHelperCannotAuthorizeCatalogConversion(t *testing.T) {
	db, runtime, exe := startupFixture(t)
	before, _ := os.ReadFile(db)
	guard, err := CheckStartupCompatibility(db, runtime, exe, nil)
	if err != nil || guard.CanActivateCatalog() {
		t.Fatalf("old helper falsely accepted: %v %+v", err, guard)
	}
	if err := guard.EnsureCatalogFormat(); ErrorCode(err) != CodeCompatibilityRequired {
		t.Fatalf("expected hard activation refusal: %v", err)
	}
	if _, err := os.Stat(db + ".catalog-format.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refusal wrote floor: %v", err)
	}
	after, _ := os.ReadFile(db)
	if !bytes.Equal(before, after) {
		t.Fatal("preflight mutated database")
	}
}

func TestCatalogCapabilityIsBoundToExactStartBinaryAndDatabase(t *testing.T) {
	db, runtime, exe := startupFixture(t)
	receipt := writeReceipt(t, db, runtime, exe, false)
	guard, err := CheckStartupCompatibility(db, runtime, exe, nil)
	if err != nil || guard.CanActivateCatalog() {
		t.Fatalf("unacknowledged pending receipt accepted: %v", err)
	}
	guard, err = CheckStartupCompatibility(db, runtime, exe, []string{CompatibilityFlag, receipt.Nonce})
	if err != nil || !guard.CanActivateCatalog() {
		t.Fatalf("bound handoff refused: %v", err)
	}
	wrong, _ := newOperationID()
	if _, err := CheckStartupCompatibility(db, runtime, exe, []string{CompatibilityFlag, wrong}); ErrorCode(err) != CodeCompatibilityRequired {
		t.Fatalf("wrong proof accepted: %v", err)
	}
	if _, err := CheckStartupCompatibility(db, runtime, exe, []string{CompatibilityFlag, receipt.Nonce, CompatibilityFlag, receipt.Nonce}); ErrorCode(err) != CodeCompatibilityRequired {
		t.Fatalf("duplicate proof accepted: %v", err)
	}
	otherDB := filepath.Join(runtime, "other.db")
	if err := os.WriteFile(otherDB, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	guard, err = CheckStartupCompatibility(otherDB, runtime, exe, []string{CompatibilityFlag, receipt.Nonce})
	if err != nil || guard.CanActivateCatalog() {
		t.Fatalf("cross-DB proof accepted: %v", err)
	}
	if err := os.WriteFile(exe, []byte("different binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	guard, err = CheckStartupCompatibility(db, runtime, exe, []string{CompatibilityFlag, receipt.Nonce})
	if err != nil || guard.CanActivateCatalog() {
		t.Fatalf("cross-binary proof accepted: %v", err)
	}
}

func TestCatalogFormatFloorPersistsBeforeActivationAndSurvivesRestart(t *testing.T) {
	db, runtime, exe := startupFixture(t)
	receipt := writeReceipt(t, db, runtime, exe, false)
	guard, err := CheckStartupCompatibility(db, runtime, exe, []string{CompatibilityFlag, receipt.Nonce})
	if err != nil {
		t.Fatal(err)
	}
	if err := guard.ValidateDatabaseFormat(1); ErrorCode(err) != CodeFormatIncompatible {
		t.Fatalf("DB activated without durable floor accepted: %v", err)
	}
	if err := guard.EnsureCatalogFormat(); err != nil {
		t.Fatal(err)
	}
	if err := guard.EnsureCatalogFormat(); err != nil {
		t.Fatal(err)
	}
	if err := guard.ValidateDatabaseFormat(0); err != nil {
		t.Fatalf("crash before DB activation not recoverable: %v", err)
	}
	if err := guard.ValidateDatabaseFormat(1); err != nil {
		t.Fatal(err)
	}
	if err := guard.ValidateDatabaseFormat(2); ErrorCode(err) != CodeFormatIncompatible {
		t.Fatalf("future DB format accepted: %v", err)
	}
	restarted, err := CheckStartupCompatibility(db, runtime, exe, nil)
	if err != nil || !restarted.CanActivateCatalog() {
		t.Fatalf("protected restart refused: %v", err)
	}
	if err := atomicWriteJSON(db+".catalog-format.json", formatFloor{Schema: 1, Catalog: 2}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckStartupCompatibility(db, runtime, exe, nil); ErrorCode(err) != CodeFormatIncompatible {
		t.Fatalf("future floor accepted: %v", err)
	}
	if err := guard.EnsureCatalogFormat(); ErrorCode(err) != CodeFormatIncompatible {
		t.Fatalf("floor lowered: %v", err)
	}
}

func TestCleanInstallAndCompletedHandoffCanActivate(t *testing.T) {
	db, runtime, exe := startupFixture(t)
	fresh := filepath.Join(runtime, "fresh.db")
	guard, err := CheckStartupCompatibility(fresh, runtime, exe, nil)
	if err != nil || !guard.CanActivateCatalog() {
		t.Fatalf("clean install blocked: %v", err)
	}
	if _, err := os.Stat(fresh); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preflight created database")
	}
	writeReceipt(t, db, runtime, exe, true)
	guard, err = CheckStartupCompatibility(db, runtime, exe, nil)
	if err != nil || !guard.CanActivateCatalog() {
		t.Fatalf("completed compatible handoff blocked: %v", err)
	}
}

func TestMalformedFormatStateFailsClosed(t *testing.T) {
	for _, payload := range []string{`{}`, `{"schema":1,"catalog":0}`, `{"schema":2,"catalog":1}`, `{"schema":1,"catalog":1,"unknown":true}`, `not json`} {
		t.Run(payload, func(t *testing.T) {
			db, runtime, exe := startupFixture(t)
			if err := os.WriteFile(db+".catalog-format.json", []byte(payload), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := CheckStartupCompatibility(db, runtime, exe, nil); ErrorCode(err) != CodeFormatIncompatible {
				t.Fatalf("corrupt floor accepted: %v", err)
			}
		})
	}
}

func TestRunHelperDoesNotRollbackAfterCatalogActivation(t *testing.T) {
	for _, mode := range []string{"startup_failure", "health_failure", "unreadable_floor"} {
		t.Run(mode, func(t *testing.T) {
			store, plan, planPath, sentinel := helperFixture(t)
			options := helperOptions(plan)
			if err := os.WriteFile(options.DatabasePath, []byte("unchanged database"), 0o600); err != nil {
				t.Fatal(err)
			}
			starts := 0
			process := &fakeRunningProcess{}
			options.Start = func(executable string, args []string) (RunningProcess, error) {
				starts++
				guard, err := CheckStartupCompatibility(options.DatabasePath, plan.RuntimeDirectory, executable, args)
				if err != nil {
					return nil, err
				}
				if err := guard.EnsureCatalogFormat(); err != nil {
					return nil, err
				}
				if mode == "unreadable_floor" {
					if err := os.WriteFile(options.DatabasePath+".catalog-format.json", []byte("corrupt"), 0o600); err != nil {
						return nil, err
					}
				}
				if mode == "startup_failure" {
					return nil, errors.New("failure after format activation")
				}
				return process, nil
			}
			options.Probe = func(context.Context, string) error { return errors.New("unhealthy") }
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := RunHelper(ctx, planPath, options)
			if ErrorCode(err) != CodeRollbackBlocked || starts != 1 {
				t.Fatalf("unsafe rollback attempted: %v starts=%d", err, starts)
			}
			current, _ := os.ReadFile(plan.CurrentExecutable)
			backup, _ := os.ReadFile(plan.Backup)
			if string(current) != "new-server" || string(backup) != "old-server" {
				t.Fatal("failure evidence was overwritten")
			}
			state, _ := store.LoadState()
			if state.Phase != PhaseFailed || state.ErrorCode != CodeRollbackBlocked {
				t.Fatalf("wrong result: %+v", state)
			}
			if mode != "startup_failure" && (!process.killed || !process.waited) {
				t.Fatal("candidate not stopped")
			}
			assertSentinel(t, sentinel)
		})
	}
}

func TestBinaryCompatibilityProbeNeverExecutesLegacyCandidate(t *testing.T) {
	legacy := filepath.Join(t.TempDir(), "legacy.exe")
	if err := os.WriteFile(legacy, []byte("legacy binary without declared probe"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := verifyBinaryCompatibility(context.Background(), legacy); ErrorCode(err) != CodeCompatibilityRequired {
		t.Fatalf("legacy candidate accepted: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyBinaryCompatibility(context.Background(), executable); err != nil {
		t.Fatalf("real bounded capability subprocess failed: %v", err)
	}
}

func TestStripCompatibilityProofAndBoundOutput(t *testing.T) {
	args := StripCompatibilityArguments([]string{"--config", "server.json", CompatibilityFlag, "proof"})
	if len(args) != 2 || args[1] != "server.json" {
		t.Fatalf("proof leaked into future plan: %v", args)
	}
	writer := &limitedCompatibilityOutput{}
	if _, err := writer.Write(make([]byte, 4097)); err == nil {
		t.Fatal("unbounded command output")
	}
}
