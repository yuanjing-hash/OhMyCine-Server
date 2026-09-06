package updater

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func initializedOriginFixture(t *testing.T) (string, string, string, *CatalogCompatibility) {
	t.Helper()
	runtime := t.TempDir()
	db := filepath.Join(runtime, "data", "server.db")
	exe := filepath.Join(runtime, "server.exe")
	if err := os.WriteFile(exe, []byte("initializing compatible binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	guard, err := CheckStartupCompatibility(db, runtime, exe, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := guard.ReserveFreshDatabase(); err != nil {
		t.Fatal(err)
	}
	// In-place data writes preserve the database file identity, like SQLite.
	if err := os.WriteFile(db, []byte("initialized database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := guard.CompleteInitialization(); err != nil {
		t.Fatal(err)
	}
	return db, runtime, exe, guard
}

func TestInitializedOriginSurvivesRestartWithoutFormatActivation(t *testing.T) {
	db, runtime, exe, _ := initializedOriginFixture(t)
	for _, binary := range []string{"same compatible build", "later compatible build"} {
		if err := os.WriteFile(exe, []byte(binary), 0o700); err != nil {
			t.Fatal(err)
		}
		guard, err := CheckStartupCompatibility(db, runtime, exe, nil)
		if err != nil || !guard.CanActivateCatalog() {
			t.Fatalf("genuine initialized origin lost on restart: %v", err)
		}
		if err := guard.ReserveFreshDatabase(); err != nil {
			t.Fatal(err)
		}
		if err := guard.CompleteInitialization(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(db + ".catalog-format.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("origin raised active floor: %v", err)
	}
}

func TestExistingEmptyDatabaseCannotGainInitializedOrigin(t *testing.T) {
	db, runtime, exe := startupFixture(t)
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	guard, err := CheckStartupCompatibility(db, runtime, exe, nil)
	if err != nil {
		t.Fatal(err)
	}
	if guard.CanActivateCatalog() {
		t.Fatal("existing empty database classified fresh")
	}
	if err := guard.ReserveFreshDatabase(); err != nil {
		t.Fatal(err)
	}
	if err := guard.CompleteInitialization(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(db + ".catalog-origin.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy startup minted fresh origin: %v", err)
	}
}

func TestFreshReservationRejectsDatabaseAppearingAfterPreflight(t *testing.T) {
	root := t.TempDir()
	db := filepath.Join(root, "server.db")
	guard, err := CheckStartupCompatibility(db, root, os.Args[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db, []byte("other process database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := guard.ReserveFreshDatabase(); ErrorCode(err) != CodeCompatibilityRequired {
		t.Fatalf("adopted raced database: %v", err)
	}
	if err := guard.CompleteInitialization(); ErrorCode(err) != CodeCompatibilityRequired {
		t.Fatalf("unreserved DB minted origin: %v", err)
	}
	payload, _ := os.ReadFile(db)
	if string(payload) != "other process database" {
		t.Fatal("raced database overwritten")
	}
}

func TestFreshInitializationFailureDoesNotMintOrigin(t *testing.T) {
	root := t.TempDir()
	db := filepath.Join(root, "server.db")
	guard, err := CheckStartupCompatibility(db, root, os.Args[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := guard.ReserveFreshDatabase(); err != nil {
		t.Fatal(err)
	}
	if err := guard.EnsureCatalogFormat(); ErrorCode(err) != CodeCompatibilityRequired {
		t.Fatalf("incomplete initialization activated format: %v", err)
	}
	// Simulate restart before successful initialization; the existing empty
	// reservation is deliberately not proof of a completed clean install.
	restarted, err := CheckStartupCompatibility(db, root, os.Args[0], nil)
	if err != nil || restarted.CanActivateCatalog() {
		t.Fatalf("failed initialization became successful provenance: %v", err)
	}
}

func TestInitializedOriginRejectsCorruptionCopyReplacementAndMissingProof(t *testing.T) {
	for _, scenario := range []string{"corrupt", "other_runtime", "replace_database", "missing"} {
		t.Run(scenario, func(t *testing.T) {
			db, runtime, exe, _ := initializedOriginFixture(t)
			switch scenario {
			case "corrupt":
				if err := os.WriteFile(db+".catalog-origin.json", []byte(`{}`), 0o600); err != nil {
					t.Fatal(err)
				}
			case "other_runtime":
				runtime = t.TempDir()
			case "replace_database":
				if err := os.Rename(db, db+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(db, []byte("another DB at same path"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(db + ".catalog-origin.json"); err != nil {
					t.Fatal(err)
				}
			}
			guard, err := CheckStartupCompatibility(db, runtime, exe, nil)
			if scenario == "missing" {
				if err != nil || guard.CanActivateCatalog() {
					t.Fatalf("missing proof permitted conversion: %v", err)
				}
			} else if ErrorCode(err) != CodeCompatibilityRequired {
				t.Fatalf("invalid provenance accepted: %v", err)
			}
		})
	}
}
