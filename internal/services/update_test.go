package services

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/buildinfo"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/updater"
)

type fakeUpdateClient struct {
	mu         sync.Mutex
	latest     updater.SelectedRelease
	latestErr  error
	started    chan struct{}
	release    chan struct{}
	prepared   updater.PreparedUpdate
	prepareErr error
}

func (f *fakeUpdateClient) Latest(context.Context, updater.Channel, string, string) (updater.SelectedRelease, error) {
	if f.started != nil {
		select {
		case f.started <- struct{}{}:
		default:
		}
	}
	if f.release != nil {
		<-f.release
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.latest, f.latestErr
}

func (f *fakeUpdateClient) Prepare(context.Context, updater.SelectedRelease, *updater.Store, updater.PrepareRequest) (updater.PreparedUpdate, error) {
	return f.prepared, f.prepareErr
}

func TestUpdateServiceRequiresAdminAndSerializesChecks(t *testing.T) {
	service, client, _, _ := testUpdateService(t)
	if _, err := service.Status(Actor{}); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("status error=%v", err)
	}
	client.started = make(chan struct{}, 1)
	client.release = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := service.Check(context.Background(), updateAdminActor(), RequestContext{})
		done <- err
	}()
	<-client.started
	if _, err := service.Check(context.Background(), updateAdminActor(), RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("concurrent check error=%v", err)
	}
	close(client.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUpdateServiceCheckSettingsAndSafeStatus(t *testing.T) {
	service, _, store, _ := testUpdateService(t)
	status, err := service.Check(context.Background(), updateAdminActor(), RequestContext{RequestID: "request-1"})
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase != updater.PhaseAvailable || status.LatestVersion != "1.1.0" || !status.UpdateAvailable || !status.InstallEnabled || status.LastCheckedAt == nil {
		t.Fatalf("unexpected status: %+v", status)
	}
	payload, _ := json.Marshal(status)
	if strings.Contains(string(payload), store.RuntimeDirectory()) || strings.Contains(string(payload), "github.com") {
		t.Fatalf("status leaked internal path or URL: %s", payload)
	}
	updated, err := service.UpdateSettings(updateAdminActor(), UpdateSettingsInput{Channel: "stable", Revision: status.Revision}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Channel != "stable" || updated.Revision != status.Revision+1 || updated.LastCheckedAt == nil {
		t.Fatalf("unexpected settings result: %+v", updated)
	}
	if _, err := service.UpdateSettings(updateAdminActor(), UpdateSettingsInput{Channel: "beta", Revision: status.Revision}, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("stale revision error=%v", err)
	}
}

func TestUpdateServiceInstallStartsHelperThenRequestsShutdown(t *testing.T) {
	service, client, store, executable := testUpdateService(t)
	status, err := service.Check(context.Background(), updateAdminActor(), RequestContext{})
	if err != nil || !status.InstallEnabled {
		t.Fatalf("check status=%+v err=%v", status, err)
	}
	helper := filepath.Join(t.TempDir(), "helper.exe")
	client.prepared = updater.PreparedUpdate{OperationID: strings.Repeat("a", 32), Version: "1.1.0", HelperExecutable: helper, PlanPath: filepath.Join(store.RuntimeDirectory(), "updates", "plans", strings.Repeat("a", 32)+".json")}
	shutdown := make(chan struct{}, 1)
	service.requestShutdown = func() { shutdown <- struct{}{} }
	launched := make(chan struct{}, 1)
	launchError := make(chan string, 1)
	service.launchHelper = func(gotHelper, gotPlan string) error {
		if gotHelper != helper || !strings.HasSuffix(gotPlan, ".json") || executable == "" {
			launchError <- gotHelper + " " + gotPlan
		}
		launched <- struct{}{}
		return nil
	}
	accepted, err := service.Install(updateAdminActor(), UpdateInstallInput{TargetVersion: "1.1.0"}, RequestContext{})
	if err != nil || accepted.Phase != updater.PhaseDownloading {
		t.Fatalf("accepted=%+v err=%v", accepted, err)
	}
	select {
	case <-launched:
	case <-time.After(2 * time.Second):
		t.Fatal("helper was not launched")
	}
	select {
	case detail := <-launchError:
		t.Fatalf("unexpected helper launch %s", detail)
	default:
	}
	select {
	case <-shutdown:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown was not requested")
	}
	state, err := store.LoadState()
	if err != nil || state.Phase != updater.PhaseWaitingForExit || state.LastCheckedAt == nil {
		t.Fatalf("state=%+v err=%v", state, err)
	}
}

func TestUpdateStatusKeepsLastCheckSeparateFromInstallCompletion(t *testing.T) {
	service, _, store, _ := testUpdateService(t)
	checked := time.Date(2026, 8, 31, 1, 2, 3, 0, time.UTC)
	completed := checked.Add(time.Hour)
	if err := store.SaveState(updater.State{Phase: updater.PhaseSucceeded, CurrentVersion: "1.0.0", TargetVersion: "1.1.0", Channel: updater.ChannelBeta, OperationID: strings.Repeat("b", 32), LastCheckedAt: &checked, CompletedAt: &completed}); err != nil {
		t.Fatal(err)
	}
	status, err := service.Status(updateAdminActor())
	if err != nil {
		t.Fatal(err)
	}
	if status.LastCheckedAt == nil || !status.LastCheckedAt.Equal(checked) || status.LastCheckedAt.Equal(completed) {
		t.Fatalf("last_checked_at=%v completed=%v", status.LastCheckedAt, completed)
	}
}

func TestNewUpdateServiceRejectsMissingShutdownCoordinator(t *testing.T) {
	_, err := NewUpdateService(t.TempDir(), "http://127.0.0.1:3000/api/v1/health", nil, zerolog.Nop(), nil)
	if ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("error=%v code=%s", err, ErrorCode(err))
	}
}

func TestUpdateServiceRejectsMutationWhileHelperOwnsPersistedOperation(t *testing.T) {
	service, _, store, _ := testUpdateService(t)
	started := time.Now().UTC()
	if err := store.SaveState(updater.State{
		Phase: updater.PhaseVerifying, CurrentVersion: "1.0.0", TargetVersion: "1.1.0",
		Channel: updater.ChannelBeta, OperationID: strings.Repeat("c", 32), StartedAt: &started,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Check(context.Background(), updateAdminActor(), RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("cross-process helper phase was not serialized: %v", err)
	}
	if _, err := service.UpdateSettings(updateAdminActor(), UpdateSettingsInput{Channel: "stable", Revision: 1}, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("settings changed while helper was active: %v", err)
	}
}

func testUpdateService(t *testing.T) (*UpdateService, *fakeUpdateClient, *updater.Store, string) {
	t.Helper()
	runtimeDirectory := t.TempDir()
	store, err := updater.NewStore(runtimeDirectory)
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(runtimeDirectory, "bin", "ohmycine-server.exe")
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	version, _ := buildinfo.ParseVersion("1.1.0")
	client := &fakeUpdateClient{latest: updater.SelectedRelease{Version: version}}
	service := newUpdateService(nil, zerolog.Nop(), updateServiceOptions{
		store: store, client: client, info: buildinfo.Info{Version: "1.0.0", Commit: strings.Repeat("a", 40), Official: true, Comparable: true},
		executable: executable, healthURL: "http://127.0.0.1:3000/api/v1/health", parentPID: 1,
		goos: "windows", goarch: "amd64", container: func() bool { return false },
		launchHelper: func(string, string) error { return nil }, requestShutdown: func() {}, now: func() time.Time { return time.Now().UTC() }, backgroundTimeout: time.Second,
	})
	return service, client, store, executable
}

func updateAdminActor() Actor {
	return Actor{User: models.User{ID: 1}, Permissions: map[string]struct{}{authz.PermissionSystemAdmin: {}}}
}
