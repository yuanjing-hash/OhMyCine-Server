package updater

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreSettingsCASAndSafeState(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	settings, err := store.LoadSettings()
	if err != nil || settings.Channel != ChannelBeta || settings.Revision != 1 {
		t.Fatalf("unexpected defaults: %+v err=%v", settings, err)
	}
	updated, err := store.UpdateSettings(ChannelStable, 1)
	if err != nil || updated.Channel != ChannelStable || updated.Revision != 2 {
		t.Fatalf("unexpected settings: %+v err=%v", updated, err)
	}
	if _, err := store.UpdateSettings(ChannelBeta, 1); ErrorCode(err) != CodeRevisionConflict {
		t.Fatalf("expected conflict, got %v", err)
	}
	checked := time.Now().UTC().Truncate(time.Millisecond)
	state := State{Phase: PhaseAvailable, CurrentVersion: "1.0.0", TargetVersion: "1.1.0", Channel: ChannelStable, LastCheckedAt: &checked}
	if err := store.SaveState(state); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadState()
	if err != nil || loaded.TargetVersion != "1.1.0" || loaded.UpdatedAt.IsZero() || loaded.LastCheckedAt == nil || !loaded.LastCheckedAt.Equal(checked) {
		t.Fatalf("unexpected state: %+v err=%v", loaded, err)
	}
}

func TestOperationHelperUsesNativeExecutableSuffix(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	windowsNames, _ := AssetNames("1.2.3", "windows", "amd64")
	windowsPaths, err := store.CreateOperation(windowsNames)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(windowsPaths.Helper) != "ohmycine-server-helper.exe" {
		t.Fatalf("unexpected Windows helper name: %s", windowsPaths.Helper)
	}
	linuxNames, _ := AssetNames("1.2.3", "linux", "amd64")
	linuxPaths, err := store.CreateOperation(linuxNames)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(linuxPaths.Helper) != "ohmycine-server-helper" {
		t.Fatalf("unexpected Linux helper name: %s", linuxPaths.Helper)
	}
}

func TestPlanValidationKeepsCandidatesInsideUpdates(t *testing.T) {
	runtimeRoot := t.TempDir()
	store, err := NewStore(runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	names, _ := AssetNames("1.2.3", "windows", "amd64")
	paths, err := store.CreateOperation(names)
	if err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(runtimeRoot, "bin", "ohmycine-server.exe")
	if err := os.MkdirAll(filepath.Dir(current), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(current, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Candidate, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	digest, _ := hashFile(current, MaxCandidateBytes)
	plan := Plan{OperationID: paths.OperationID, TargetVersion: "1.2.3", RuntimeDirectory: store.RuntimeDirectory(), CurrentExecutable: current, CurrentSHA256: digest, Candidate: paths.Candidate, Backup: paths.Backup, ParentPID: os.Getpid(), HealthURL: "http://127.0.0.1:3000/api/v1/health", ParentWaitMillis: 5000, HealthWaitMillis: 5000, CreatedAt: time.Now().UTC()}
	planPath, err := store.SavePlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadPlanFile(planPath)
	if err != nil || loaded.OperationID != plan.OperationID {
		t.Fatalf("unexpected plan: %+v err=%v", loaded, err)
	}
	plan.Candidate = filepath.Join(runtimeRoot, "database.sqlite")
	if _, err := store.SavePlan(plan); ErrorCode(err) != CodePlanInvalid {
		t.Fatalf("expected path rejection, got %v", err)
	}
	plan.Candidate = paths.Candidate
	plan.HealthURL = "http://example.com/api/v1/health"
	if _, err := store.SavePlan(plan); ErrorCode(err) != CodePlanInvalid {
		t.Fatalf("expected health URL rejection, got %v", err)
	}
}
