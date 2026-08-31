package updater

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeRunningProcess struct {
	killed   bool
	waited   bool
	released bool
}

func (p *fakeRunningProcess) Kill() error    { p.killed = true; return nil }
func (p *fakeRunningProcess) Wait() error    { p.waited = true; return nil }
func (p *fakeRunningProcess) Release() error { p.released = true; return nil }

func helperFixture(t *testing.T) (*Store, Plan, string, string) {
	t.Helper()
	runtimeRoot := t.TempDir()
	store, err := NewStore(runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	names, _ := AssetNames("2.0.0", "windows", "amd64")
	paths, err := store.CreateOperation(names)
	if err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(runtimeRoot, "bin", "ohmycine-server.exe")
	if err := os.MkdirAll(filepath.Dir(current), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(current, []byte("old-server"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Candidate, []byte("new-server"), 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(runtimeRoot, "data", "credentials.sentinel")
	if err := os.MkdirAll(filepath.Dir(sentinel), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("never-touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, _ := hashFile(current, MaxCandidateBytes)
	plan := Plan{OperationID: paths.OperationID, TargetVersion: "2.0.0", RuntimeDirectory: runtimeRoot, CurrentExecutable: current, CurrentSHA256: digest, Candidate: paths.Candidate, Backup: paths.Backup, ParentPID: os.Getpid(), HealthURL: "http://127.0.0.1:3000/api/v1/health", ParentWaitMillis: 1000, HealthWaitMillis: 1000, CreatedAt: time.Now().UTC()}
	planPath, err := store.SavePlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	return store, plan, planPath, sentinel
}

func helperOptions(plan Plan) HelperOptions {
	return HelperOptions{
		ResolveExecutable: func(int) (string, error) { return plan.CurrentExecutable, nil },
		WaitForExit:       func(context.Context, int) error { return nil },
		Start:             func(string, []string) (RunningProcess, error) { return &fakeRunningProcess{}, nil },
		Probe:             func(context.Context, string) error { return nil },
	}
}

func assertSentinel(t *testing.T, sentinel string) {
	t.Helper()
	payload, err := os.ReadFile(sentinel)
	if err != nil || string(payload) != "never-touch" {
		t.Fatalf("runtime sentinel changed: %q err=%v", payload, err)
	}
}

func TestRunHelperSuccessPreservesRuntimeData(t *testing.T) {
	store, plan, planPath, sentinel := helperFixture(t)
	if err := RunHelper(context.Background(), planPath, helperOptions(plan)); err != nil {
		t.Fatal(err)
	}
	current, _ := os.ReadFile(plan.CurrentExecutable)
	backup, _ := os.ReadFile(plan.Backup)
	if string(current) != "new-server" || string(backup) != "old-server" {
		t.Fatalf("unexpected replacement current=%q backup=%q", current, backup)
	}
	state, err := store.LoadState()
	if err != nil || state.Phase != PhaseSucceeded || state.CurrentVersion != "2.0.0" || state.CompletedAt == nil {
		t.Fatalf("unexpected success state: %+v err=%v", state, err)
	}
	assertSentinel(t, sentinel)
}

func TestRunHelperRestartFailureRollsBack(t *testing.T) {
	store, plan, planPath, sentinel := helperFixture(t)
	options := helperOptions(plan)
	starts := 0
	options.Start = func(string, []string) (RunningProcess, error) {
		starts++
		if starts == 1 {
			return nil, errors.New("new process failed")
		}
		return &fakeRunningProcess{}, nil
	}
	err := RunHelper(context.Background(), planPath, options)
	if ErrorCode(err) != CodeRestartFailed {
		t.Fatalf("expected restart failure, got %v", err)
	}
	current, _ := os.ReadFile(plan.CurrentExecutable)
	if string(current) != "old-server" || starts != 2 {
		t.Fatalf("rollback did not restore old server: current=%q starts=%d", current, starts)
	}
	state, _ := store.LoadState()
	if state.Phase != PhaseRolledBack || state.ErrorCode != CodeRestartFailed {
		t.Fatalf("unexpected rollback state: %+v", state)
	}
	assertSentinel(t, sentinel)
}

func TestRunHelperHealthFailureStopsCandidateAndRollsBack(t *testing.T) {
	store, plan, planPath, sentinel := helperFixture(t)
	options := helperOptions(plan)
	newProcess := &fakeRunningProcess{}
	starts := 0
	options.Start = func(string, []string) (RunningProcess, error) {
		starts++
		if starts == 1 {
			return newProcess, nil
		}
		return &fakeRunningProcess{}, nil
	}
	options.Probe = func(context.Context, string) error { return errors.New("not healthy") }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := RunHelper(ctx, planPath, options)
	if ErrorCode(err) != CodeHealthCheckFailed {
		t.Fatalf("expected health failure, got %v", err)
	}
	if !newProcess.killed || !newProcess.waited || starts != 2 {
		t.Fatalf("new process was not stopped before rollback: %+v starts=%d", newProcess, starts)
	}
	current, _ := os.ReadFile(plan.CurrentExecutable)
	if string(current) != "old-server" {
		t.Fatalf("old server was not restored: %q", current)
	}
	state, _ := store.LoadState()
	if state.Phase != PhaseRolledBack || state.ErrorCode != CodeHealthCheckFailed {
		t.Fatalf("unexpected health rollback state: %+v", state)
	}
	assertSentinel(t, sentinel)
}

func TestRunHelperRejectsParentExecutableMismatchBeforeMutation(t *testing.T) {
	_, plan, planPath, sentinel := helperFixture(t)
	options := helperOptions(plan)
	other := filepath.Join(filepath.Dir(plan.CurrentExecutable), "other.exe")
	if err := os.WriteFile(other, []byte("other"), 0o700); err != nil {
		t.Fatal(err)
	}
	options.ResolveExecutable = func(int) (string, error) { return other, nil }
	if err := RunHelper(context.Background(), planPath, options); ErrorCode(err) != CodePlanInvalid {
		t.Fatalf("expected parent binding rejection, got %v", err)
	}
	current, _ := os.ReadFile(plan.CurrentExecutable)
	if string(current) != "old-server" {
		t.Fatalf("current executable changed before rejection: %q", current)
	}
	assertSentinel(t, sentinel)
}

func TestRunHelperRejectsUnresolvedParentBeforeMutation(t *testing.T) {
	_, plan, planPath, sentinel := helperFixture(t)
	options := helperOptions(plan)
	options.ResolveExecutable = func(int) (string, error) { return "", errors.New("process unavailable") }
	if err := RunHelper(context.Background(), planPath, options); ErrorCode(err) != CodePlanInvalid {
		t.Fatalf("expected unresolved parent rejection, got %v", err)
	}
	current, _ := os.ReadFile(plan.CurrentExecutable)
	if string(current) != "old-server" {
		t.Fatalf("current executable changed before rejection: %q", current)
	}
	assertSentinel(t, sentinel)
}

func TestRunHelperRestartsUnchangedServerWhenInitialBackupFails(t *testing.T) {
	store, plan, planPath, sentinel := helperFixture(t)
	options := helperOptions(plan)
	starts := 0
	options.Start = func(executable string, _ []string) (RunningProcess, error) {
		starts++
		if executable != plan.CurrentExecutable {
			t.Fatalf("restarted executable=%q want=%q", executable, plan.CurrentExecutable)
		}
		return &fakeRunningProcess{}, nil
	}
	options.Rename = func(source, destination string) error {
		if source == plan.CurrentExecutable && destination == plan.Backup {
			return errors.New("backup failed")
		}
		return os.Rename(source, destination)
	}
	err := RunHelper(context.Background(), planPath, options)
	if ErrorCode(err) != CodeReplacementFailed || starts != 1 {
		t.Fatalf("error=%v starts=%d", err, starts)
	}
	current, _ := os.ReadFile(plan.CurrentExecutable)
	if string(current) != "old-server" {
		t.Fatalf("unchanged Server was not preserved: %q", current)
	}
	state, _ := store.LoadState()
	if state.Phase != PhaseFailed || state.ErrorCode != CodeReplacementFailed {
		t.Fatalf("unexpected failure state: %+v", state)
	}
	assertSentinel(t, sentinel)
}

func TestDefaultProbeRequiresExactHealthEnvelope(t *testing.T) {
	for name, payload := range map[string]string{
		"html":  `<html>ok</html>`,
		"wrong": `{"code":0,"data":{"status":"starting"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write([]byte(payload)) }))
			defer server.Close()
			if err := defaultProbe(context.Background(), server.URL); err == nil {
				t.Fatal("invalid health response was accepted")
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"code":0,"message":"success","data":{"status":"ok"}}`))
	}))
	defer server.Close()
	if err := defaultProbe(context.Background(), server.URL); err != nil {
		t.Fatalf("valid health response rejected: %v", err)
	}
}
