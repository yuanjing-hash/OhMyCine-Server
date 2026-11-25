package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"gorm.io/gorm"
)

// Drive the actual nonfailure RetryLater/Claim boundary without a wall clock
// sleep. This is a new execution lease, never a second provider submission.
func finishScheduledDownload(t *testing.T, worker *DownloadWorker, queue *QueueService, claim *ClaimedJob) WorkerResult {
	t.Helper()
	for samples := 0; samples < 16; samples++ {
		result := worker.Run(context.Background(), workerRuntime{queue: queue, job: *claim}, *claim)
		if result.RetryAt == nil || result.ErrorCode != "" {
			return result
		}
		if err := queue.RetryLater(claim.Job.ID, claim.LeaseToken, "", "", *result.RetryAt); err != nil {
			t.Fatal(err)
		}
		clock, ok := queue.clock.(*fakeQueueClock)
		if !ok {
			t.Fatal("fixture needs an injected clock")
		}
		clock.now = *result.RetryAt
		if err := queue.PromoteDueRetries(); err != nil {
			t.Fatal(err)
		}
		next, err := queue.Claim([]string{"download"})
		if err != nil || next == nil || next.Job.ID != claim.Job.ID {
			t.Fatalf("observation claim=%+v err=%v", next, err)
		}
		old := *claim
		if next.Attempt.ID != old.Attempt.ID || next.Job.AttemptCount != old.Job.AttemptCount || next.LeaseToken == old.LeaseToken {
			t.Fatal("ordinary observation did not retain logical attempt with a fresh lease")
		}
		if err := queue.Heartbeat(old.Job.ID, old.LeaseToken, nil, nil, nil, nil, nil); ErrorCode(err) != CodeQueueLeaseInvalid {
			t.Fatalf("old observation heartbeat=%v", err)
		}
		if err := queue.Complete(old.Job.ID, old.LeaseToken); ErrorCode(err) != CodeQueueLeaseInvalid {
			t.Fatalf("old observation completed new lease=%v", err)
		}
		*claim = *next
	}
	t.Fatal("download did not settle within bounded observations")
	return WorkerResult{}
}

type stubDownloadClient struct {
	mu           sync.Mutex
	gets         int
	getCalls     int
	submits      int
	source       downloadpkg.Source
	paused       bool
	resumed      bool
	cancelled    bool
	deleteData   bool
	cancelErr    error
	cancelIDs    []string
	cancelHook   func(string)
	getErr       error
	getErrors    []error
	seedTask     *downloadpkg.Task
	cleanupErr   error
	cleanupHook  func(string)
	deletedTags  []string
	submitWait   <-chan struct{}
	submitSeen   chan<- struct{}
	submitErr    error
	reconcileErr error
	reconciles   int
}

// Only native-route fixtures declare this contract. Ordinary qB fixtures keep
// their existing BT-only client, while mixed shares/adoption remain nonoffline.
type sourceAwareNativeStub struct {
	*stubDownloadClient
	namespace string
}

func (c *sourceAwareNativeStub) OfflineOperation(source downloadpkg.Source) downloadpkg.OfflineOperation {
	if source.Kind != downloadpkg.SourceURL && source.Kind != downloadpkg.SourceTorrent {
		return downloadpkg.OfflineOperation{}
	}
	return downloadpkg.OfflineOperation{Kind: downloadpkg.OfflineOperationCloud, AccountNamespace: c.namespace, KeepOutputStop: true}
}
func (c *sourceAwareNativeStub) ObserveOffline(ctx context.Context, id string) (downloadpkg.Task, error) {
	return c.Get(ctx, id)
}

type downloadProviderDiagnosticError struct{}

func (downloadProviderDiagnosticError) Error() string { return "private-output-id" }
func (downloadProviderDiagnosticError) ProviderFailureDetails() (string, int, int, string) {
	return "reconcile", 200, 12345, "provider_error"
}

type metadataDownloadClient struct {
	*stubDownloadClient
	metadataOnly      bool
	manifestCalls     int
	manifestErr       error
	category          string
	categoryPath      string
	routedPath        string
	categories        []downloadpkg.Category
	categoryCalls     []string
	keepCategoryPath  bool
	updateCategoryErr error
	setCategoryErr    error
}

type providerWakeRuntime struct {
	heartbeats chan struct{}
	err        error
}

func (r *providerWakeRuntime) Heartbeat(*float64, *int64, *int64, *float64, *int64) error {
	if r.heartbeats != nil {
		select {
		case r.heartbeats <- struct{}{}:
		default:
		}
	}
	return r.err
}

func (*providerWakeRuntime) Checkpoint(any) error { return nil }

func (c *metadataDownloadClient) Submit(ctx context.Context, request downloadpkg.SubmitRequest) (downloadpkg.Task, error) {
	c.metadataOnly = request.MetadataOnly
	return c.stubDownloadClient.Submit(ctx, request)
}

func (c *metadataDownloadClient) Manifest(context.Context, string) (downloadpkg.Manifest, error) {
	c.mu.Lock()
	c.manifestCalls++
	err := c.manifestErr
	c.mu.Unlock()
	if err != nil {
		return downloadpkg.Manifest{}, err
	}
	return downloadpkg.Manifest{Name: "Example.Show.S01E01", Complete: true, Files: []downloadpkg.File{{RelativePath: "Example.Show.S01E01/Example.Show.S01E01.mkv", Size: 2 * 1024 * 1024 * 1024}}}, nil
}
func (c *metadataDownloadClient) Categories(context.Context) ([]downloadpkg.Category, error) {
	c.categoryCalls = append(c.categoryCalls, "categories")
	return append([]downloadpkg.Category(nil), c.categories...), nil
}
func (c *metadataDownloadClient) EnsureCategory(_ context.Context, name, savePath string) error {
	c.categoryCalls = append(c.categoryCalls, "create")
	c.category, c.categoryPath = name, savePath
	c.categories = append(c.categories, downloadpkg.Category{Name: name, SavePath: savePath})
	return nil
}
func (c *metadataDownloadClient) UpdateCategory(_ context.Context, name, savePath string) error {
	c.categoryCalls = append(c.categoryCalls, "update")
	if c.updateCategoryErr != nil {
		return c.updateCategoryErr
	}
	c.category, c.categoryPath = name, savePath
	if !c.keepCategoryPath {
		for index := range c.categories {
			if strings.EqualFold(c.categories[index].Name, name) {
				c.categories[index].SavePath = savePath
			}
		}
	}
	return nil
}
func (c *metadataDownloadClient) SetCategory(_ context.Context, _ string, category, savePath string) error {
	c.categoryCalls = append(c.categoryCalls, "set")
	if c.setCategoryErr != nil {
		return c.setCategoryErr
	}
	c.category, c.routedPath = category, savePath
	return nil
}

func (c *metadataDownloadClient) Resume(ctx context.Context, id string) error {
	c.categoryCalls = append(c.categoryCalls, "resume")
	return c.stubDownloadClient.Resume(ctx, id)
}

func (c *stubDownloadClient) Test(context.Context) (downloadpkg.Health, error) {
	return downloadpkg.Health{Version: "stub-v1"}, nil
}
func (c *stubDownloadClient) Submit(_ context.Context, request downloadpkg.SubmitRequest) (downloadpkg.Task, error) {
	if c.submitSeen != nil {
		select {
		case c.submitSeen <- struct{}{}:
		default:
		}
	}
	if c.submitWait != nil {
		<-c.submitWait
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.submits++
	c.source = request.Source
	if c.submitErr != nil {
		return downloadpkg.Task{}, c.submitErr
	}
	return downloadpkg.Task{ID: "provider-hash", Status: "downloading"}, nil
}
func (c *stubDownloadClient) ReconcileSubmission(_ context.Context, request downloadpkg.SubmitRequest) (downloadpkg.Task, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reconciles++
	if c.reconcileErr != nil {
		return downloadpkg.Task{}, c.reconcileErr
	}
	if request.Source.URL != c.source.URL {
		return downloadpkg.Task{}, errors.New("reconciliation source changed")
	}
	return downloadpkg.Task{ID: "provider-hash", Status: "downloading"}, nil
}

func (c *stubDownloadClient) Get(context.Context, string) (downloadpkg.Task, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.getCalls++
	if len(c.getErrors) > 0 {
		err := c.getErrors[0]
		c.getErrors = c.getErrors[1:]
		if err != nil {
			return downloadpkg.Task{}, err
		}
	}
	if c.getErr != nil {
		return downloadpkg.Task{}, c.getErr
	}
	if c.seedTask != nil {
		return *c.seedTask, nil
	}
	c.gets++
	progress := 50.0
	if c.gets > 1 {
		progress = 100
	}
	completed, total, speed, upload, eta := int64(progress), int64(100), int64(20), int64(2), int64(3)
	return downloadpkg.Task{ID: "provider-hash", Status: map[bool]string{true: "completed", false: "downloading"}[progress == 100], Progress: &progress, BytesCompleted: &completed, BytesTotal: &total, DownloadSpeed: &speed, UploadSpeed: &upload, ETASeconds: &eta, Completed: progress == 100}, nil
}
func (c *stubDownloadClient) DeleteManagedTag(_ context.Context, tag string) error {
	c.mu.Lock()
	if c.cleanupErr != nil {
		err := c.cleanupErr
		c.mu.Unlock()
		return err
	}
	c.deletedTags = append(c.deletedTags, tag)
	hook := c.cleanupHook
	c.cleanupHook = nil
	c.mu.Unlock()
	if hook != nil {
		hook(tag)
	}
	return nil
}
func (c *stubDownloadClient) Pause(context.Context, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paused = true
	return nil
}
func (c *stubDownloadClient) Resume(context.Context, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resumed = true
	return nil
}
func (c *stubDownloadClient) Cancel(_ context.Context, id string, deleteData bool) error {
	c.mu.Lock()
	c.cancelled, c.deleteData = true, deleteData
	c.cancelIDs = append(c.cancelIDs, id)
	err, hook := c.cancelErr, c.cancelHook
	c.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	return err
}

func downloadFixture(t *testing.T) (*DownloadService, *DownloaderService, *QueueService, Actor, *stubDownloadClient) {
	t.Helper()
	queue, actor, _ := queueFixture(t)
	for _, code := range []string{authz.PermissionDownloadersRead, authz.PermissionDownloadersCreate, authz.PermissionDownloadersUpdate, authz.PermissionDownloadersDelete, authz.PermissionDownloadersTest, authz.PermissionDownloadsReadOwn, authz.PermissionDownloadsReadAll, authz.PermissionDownloadsCreate} {
		actor.Permissions[code] = struct{}{}
	}
	actor.Permissions[authz.PermissionMediaLibrariesRead] = struct{}{}
	persistFixtureAuthority(t, queue.db, actor)
	store, err := credential.Open(filepath.Join(t.TempDir(), "credentials.key"), "")
	if err != nil {
		t.Fatal(err)
	}
	client := &stubDownloadClient{}
	registry := downloadpkg.NewRegistry()
	capabilities := downloadpkg.Capabilities{Pause: true, Resume: true, Cancel: true, DownloadSpeed: true, UploadSpeed: true, ETA: true, Seeding: true}
	if err := registry.Register(models.DownloaderTypeQBittorrent, capabilities, func(downloadpkg.Config) (downloadpkg.Client, error) { return client, nil }); err != nil {
		t.Fatal(err)
	}
	downloaders := NewDownloaderService(queue.db, queue.audit, store, registry)
	settings := NewDownloadSettingsService(queue.db, queue.audit)
	downloads := NewDownloadService(queue.db, queue.audit, store, downloaders, settings, queue, zerolog.Nop())
	downloads.SetSeedingSettings(NewSeedingSettingsService(queue.db, queue.audit))
	return downloads, downloaders, queue, actor, client
}

type offlineLifecycleFixtureState struct {
	mu                   sync.Mutex
	clock                *fakeQueueClock
	starts               map[string]time.Time
	ids                  map[string]string
	writes, reads, stops int
	ready                bool
	stopErr              error
	reject, terminal     bool
	removed              map[string]bool
}
type offlineLifecycleFixtureClient struct {
	state     *offlineLifecycleFixtureState
	namespace string
	plan      *downloadpkg.OfflineRequiredPlan
}

func (c *offlineLifecycleFixtureClient) Test(context.Context) (downloadpkg.Health, error) {
	return downloadpkg.Health{Version: "fixture"}, nil
}
func (c *offlineLifecycleFixtureClient) OfflineOperation(source downloadpkg.Source) downloadpkg.OfflineOperation {
	if source.Kind != downloadpkg.SourceURL && source.Kind != downloadpkg.SourceTorrent {
		return downloadpkg.OfflineOperation{}
	}
	return downloadpkg.OfflineOperation{Kind: downloadpkg.OfflineOperationCloud, AccountNamespace: c.namespace, KeepOutputStop: true, RequiredFileCompletion: true}
}
func (c *offlineLifecycleFixtureClient) BindOfflineRequired(plan downloadpkg.OfflineRequiredPlan) error {
	c.plan = &plan
	return nil
}
func (c *offlineLifecycleFixtureClient) Submit(ctx context.Context, r downloadpkg.SubmitRequest) (downloadpkg.Task, error) {
	if err := cloudpkg.RecordOfflineDispatch(ctx, c.state.clock.Now()); err != nil {
		return downloadpkg.Task{}, err
	}
	c.state.mu.Lock()
	rejected := c.state.reject
	c.state.mu.Unlock()
	if rejected {
		recorded, err := cloudpkg.RecordOfflineNonAcceptance(ctx)
		if err != nil {
			return downloadpkg.Task{}, err
		}
		if !recorded {
			return downloadpkg.Task{}, downloadpkg.Error("downloader_submit_unknown", true, nil)
		}
		return downloadpkg.Task{}, downloadpkg.WithDefiniteSubmissionNonAcceptance(downloadpkg.Error("downloader_provider_rejected", false, nil))
	}

	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	id := "native:" + r.Tag
	c.state.writes++
	c.state.starts[id] = c.state.clock.Now()
	c.state.ids[r.Tag] = id
	return downloadpkg.Task{ID: id, Status: "running"}, nil
}
func (c *offlineLifecycleFixtureClient) ReconcileSubmission(ctx context.Context, r downloadpkg.SubmitRequest) (downloadpkg.Task, error) {
	c.state.mu.Lock()
	id := c.state.ids[r.Tag]
	c.state.mu.Unlock()
	if id == "" {
		return downloadpkg.Task{}, downloadpkg.Error("downloader_submit_unknown", true, nil)
	}
	return c.Get(ctx, id)
}
func (c *offlineLifecycleFixtureClient) Get(_ context.Context, id string) (downloadpkg.Task, error) {
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	if c.state.removed[id] {
		return downloadpkg.Task{}, downloadpkg.Error("downloader_task_not_found", false, nil)
	}
	c.state.reads++
	started := c.state.starts[id]
	ready := c.state.ready
	progress := 75.0
	o := &downloadpkg.OfflineObservation{Version: 1, Kind: downloadpkg.OfflineOperationCloud, OperationID: id, AccountNamespace: c.namespace, StartedAt: &started, StartProvenance: downloadpkg.OfflineStartDispatch, Ready: ready, CompletionMode: downloadpkg.OfflineCompletionAggregate, KeepOutputStop: true}
	out := downloadpkg.Task{ID: id, Status: "running", Progress: &progress, Completed: ready, Offline: o}
	if c.state.terminal {
		o.Ready = false
		o.TerminalFailure = true
		out.Completed = false
		out.Failed = true
		out.ErrorCode = "downloader_provider_failed"
		return out, nil
	}

	if c.plan != nil && ready {
		digest, _ := c.plan.Digest()
		manifest := c.fixtureManifest(id)
		o.RequiredDigest, o.CompletionMode, o.ReadyManifest = digest, downloadpkg.OfflineCompletionRequiredFiles, &manifest
		out.Completed = false
	}
	return out, nil
}
func (c *offlineLifecycleFixtureClient) ObserveOffline(ctx context.Context, id string) (downloadpkg.Task, error) {
	return c.Get(ctx, id)
}
func (c *offlineLifecycleFixtureClient) Manifest(_ context.Context, id string) (downloadpkg.Manifest, error) {
	c.state.mu.Lock()
	removed := c.state.removed[id]
	c.state.mu.Unlock()
	if removed {
		return downloadpkg.Manifest{}, downloadpkg.Error("downloader_task_not_found", false, nil)
	}
	return c.fixtureManifest(id), nil
}
func (c *offlineLifecycleFixtureClient) fixtureManifest(id string) downloadpkg.Manifest {
	m := downloadpkg.Manifest{Name: "Feature", Complete: true}
	if c.plan != nil {
		m.Name = c.plan.RootName
		for _, f := range c.plan.Files {
			m.Files = append(m.Files, downloadpkg.File{RelativePath: f.RelativePath, Size: f.Size, ProviderItemID: id + ":" + f.RelativePath, ProviderParentID: "owned-root"})
		}
	}
	return m
}
func (c *offlineLifecycleFixtureClient) Cancel(_ context.Context, id string, deleteData bool) error {
	if deleteData {
		return errors.New("fixture forbids deleting cloud output")
	}
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	c.state.stops++
	if c.state.stopErr == nil {
		if c.state.removed == nil {
			c.state.removed = map[string]bool{}
		}
		c.state.removed[id] = true
	}
	return c.state.stopErr
}

func (c *offlineLifecycleFixtureClient) StopOfflineKeepOutput(ctx context.Context, id string) error {
	return c.Cancel(ctx, id, false)
}

func TestDormantNativeDownloadAdmissionsUnblockDistinctQueuedWork(t *testing.T) {
	downloads, _, queue, actor, _ := downloadFixture(t)
	worker := NewDownloadWorker(downloads)
	clock := queue.clock.(*fakeQueueClock)
	ctx := context.Background()
	const pan115ID, btID = "capacity-115", "capacity-bt"
	for _, record := range []models.Downloader{
		{ID: pan115ID, Name: "115", NameNormalized: "115", Type: models.DownloaderTypePan115Offline, Enabled: true, MaxConcurrentDownloads: 2},
		{ID: btID, Name: "BT", NameNormalized: "bt", Type: models.DownloaderTypeQBittorrent, Enabled: true, MaxConcurrentDownloads: 2},
	} {
		if err := queue.db.Create(&record).Error; err != nil {
			t.Fatal(err)
		}
	}
	enqueue := func(task models.DownloadTask, control string, hash string) models.DownloadTask {
		t.Helper()
		task.OwnerID, task.DisplayName = actor.User.ID, task.ID
		if task.DownloaderID == nil {
			id := pan115ID
			task.DownloaderID, task.ProviderType = &id, models.DownloaderTypePan115Offline
		}
		source := downloadSourceEnvelope{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:" + hash}
		encoded, err := json.Marshal(source)
		if err != nil {
			t.Fatal(err)
		}
		task.SourceCiphertext, err = downloads.credentials.Encrypt(downloadSourcePurpose(task.ID), string(encoded))
		if err != nil {
			t.Fatal(err)
		}
		_, err = queue.EnqueueWith(EnqueueJobInput{OwnerID: actor.User.ID, JobType: "download", DisplayName: task.ID, Payload: downloadJobPayload{DownloadTaskID: task.ID, ControlIntent: control}}, func(tx *gorm.DB, job models.Job) error {
			task.JobID = job.ID
			return tx.Create(&task).Error
		})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	seedHeld := func(task models.DownloadTask, updates map[string]any) models.Job {
		t.Helper()
		if err := queue.db.Model(&models.Job{}).Where("id = ?", task.JobID).Updates(updates).Error; err != nil {
			t.Fatal(err)
		}
		admission := models.DownloadAdmission{AcquisitionKey: downloadAcquisitionKey(task), DownloaderID: *task.DownloaderID, TaskID: task.ID, State: "held", CreatedAt: clock.Now(), UpdatedAt: clock.Now()}
		if err := queue.db.Create(&admission).Error; err != nil {
			t.Fatal(err)
		}
		var job models.Job
		if err := queue.db.First(&job, "id = ?", task.JobID).Error; err != nil {
			t.Fatal(err)
		}
		return job
	}
	assertAdmission := func(task models.DownloadTask, state string) {
		t.Helper()
		var admission models.DownloadAdmission
		if err := queue.db.First(&admission, "acquisition_key = ?", downloadAcquisitionKey(task)).Error; err != nil || admission.State != state {
			t.Fatalf("%s admission=%+v, want %s: %v", task.ID, admission, state, err)
		}
	}
	start, deadline := clock.Now().Add(-40*time.Minute), clock.Now().Add(-10*time.Minute)
	ghost := enqueue(models.DownloadTask{
		ID: "expired-native", Phase: models.DownloadTaskStatusCancelled, LastErrorCode: CodeOfflineDeadlineExceeded,
		OfflineKind: string(downloadpkg.OfflineOperationCloud), OfflineStopPending: true, OfflineStartedAt: &start, OfflineDeadlineAt: &deadline,
		OfflineStartProvenance: downloadpkg.OfflineStartDispatch, OfflineAccountNamespace: "original-account", OfflineResourceDigest: "original-resource",
	}, "cancel", strings.Repeat("1", 40))
	controlDue := clock.Now().Add(time.Minute)
	ghostJob := seedHeld(ghost, map[string]any{"status": models.JobStatusRetryWait, "next_attempt_at": controlDue, "attempt_count": 95})
	legacy := enqueue(models.DownloadTask{ID: "failed-legacy", Phase: models.DownloadTaskStatusSubmitting, LastErrorCode: CodeDownloaderStorageUnavailable}, "", strings.Repeat("2", 40))
	legacyJob := seedHeld(legacy, map[string]any{"status": models.JobStatusFailed, "last_error_code": CodeDownloaderStorageUnavailable, "attempt_count": 1970})
	btDownloader := btID
	activeBT := enqueue(models.DownloadTask{ID: "active-bt", DownloaderID: &btDownloader, ProviderType: models.DownloaderTypeQBittorrent, Phase: models.DownloadTaskStatusDownloading, ProviderTaskID: strings.Repeat("3", 40)}, "", strings.Repeat("3", 40))
	seedHeld(activeBT, map[string]any{"status": models.JobStatusRetryWait, "next_attempt_at": clock.Now().Add(time.Hour)})
	newPan := enqueue(models.DownloadTask{ID: "new-115", Phase: models.DownloadTaskStatusQueued}, "", strings.Repeat("4", 40))
	other := enqueue(models.DownloadTask{ID: "new-bt", DownloaderID: &btDownloader, ProviderType: models.DownloaderTypeQBittorrent, Phase: models.DownloadTaskStatusQueued}, "", strings.Repeat("5", 40))
	claim, err := queue.Claim([]string{"download"})
	if err != nil || claim == nil || claim.Job.ID != other.JobID {
		t.Fatalf("other downloader did not advance while 115 was full: %+v %v", claim, err)
	}
	if err := queue.RetryLater(claim.Job.ID, claim.LeaseToken, "", "", clock.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if next, err := queue.Claim([]string{"download"}); err != nil || next != nil {
		t.Fatalf("the two pending records did not occupy both old 115 units: %+v %v", next, err)
	}
	if err := worker.RecoverOfflineStops(ctx); err != nil {
		t.Fatal(err)
	}
	assertAdmission(ghost, "released")
	assertAdmission(legacy, "released")
	assertAdmission(activeBT, "held")
	for _, before := range []models.DownloadTask{ghost, legacy} {
		var task models.DownloadTask
		if err := queue.db.First(&task, "id = ?", before.ID).Error; err != nil || task.Phase != before.Phase || task.LastErrorCode != before.LastErrorCode || task.OfflineStopPending != before.OfflineStopPending || task.OfflineSettledAt != nil || task.SourceCiphertext != before.SourceCiphertext || task.ProviderTaskID != "" {
			t.Fatalf("retired acquisition facts changed for %s: %v", before.ID, err)
		}
	}
	for _, before := range []models.Job{ghostJob, legacyJob} {
		var job models.Job
		if err := queue.db.First(&job, "id = ?", before.ID).Error; err != nil || job.Status != before.Status || job.PayloadJSON != before.PayloadJSON || job.AttemptCount != before.AttemptCount {
			t.Fatalf("old Job was changed or resubmitted: %s %v", before.ID, err)
		}
	}
	claim, err = queue.Claim([]string{"download"})
	if err != nil || claim == nil || claim.Job.ID != newPan.JobID {
		t.Fatalf("existing distinct 115 magnet did not advance: %+v %v", claim, err)
	}
	assertAdmission(newPan, "held")
	if err := queue.RetryLater(claim.Job.ID, claim.LeaseToken, "", "", clock.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	clock.now = controlDue
	if err := queue.PromoteDueRetries(); err != nil {
		t.Fatal(err)
	}
	claim, err = queue.Claim([]string{"download"})
	if err != nil || claim == nil || claim.Job.ID != ghost.JobID {
		t.Fatalf("original stop did not remain runnable: %+v %v", claim, err)
	}
	// The required output can be ready while a native stop is still pending.
	native := downloadpkg.Task{ID: "late-native", Status: "running", Completed: true, Offline: &downloadpkg.OfflineObservation{
		Version: 1, Kind: downloadpkg.OfflineOperationCloud, Ready: true, StopPending: true,
	}}
	if err := worker.recordPendingControlProvider(ctx, *claim, ghost, native); err != nil {
		t.Fatal(err)
	}
	assertAdmission(ghost, "held")
	if err := queue.RetryLater(claim.Job.ID, claim.LeaseToken, downloadCancellationPending, downloadCancellationMessage, clock.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := worker.RecoverOfflineStops(ctx); err != nil {
		t.Fatal(err)
	}
	assertAdmission(ghost, "held")
	enqueue(models.DownloadTask{ID: "over-capacity-115", Phase: models.DownloadTaskStatusQueued}, "", strings.Repeat("6", 40))
	if next, err := queue.Claim([]string{"download"}); err != nil || next != nil {
		t.Fatalf("confirmed acquisition bypassed 115 capacity: %+v %v", next, err)
	}
}

type preparationControlFixture struct {
	*stubDownloadClient
	cleaned []string
}

func (c *preparationControlFixture) AbandonPreparation(_ context.Context, request downloadpkg.SubmitRequest, deleteData bool) (bool, error) {
	if request.Tag == "" {
		return false, errors.New("missing original workspace tag")
	}
	if deleteData {
		c.cleaned = append(c.cleaned, request.Tag)
	}
	return true, nil
}

func TestPreparationCancellationReclaimsCapacityAndSourceCleanupUsesControlLease(t *testing.T) {
	downloads, downloaders, queue, actor, stub := downloadFixture(t)
	actor.Permissions[authz.PermissionJobsControlOwn] = struct{}{}
	clock := queue.clock.(*fakeQueueClock)
	client := &preparationControlFixture{stubDownloadClient: stub}
	if err := downloaders.registry.Register(models.DownloaderTypeFake, downloadpkg.Capabilities{Cancel: true}, func(downloadpkg.Config) (downloadpkg.Client, error) { return client, nil }); err != nil {
		t.Fatal(err)
	}
	provider := models.Downloader{ID: "preparation-provider", Name: "Preparation", NameNormalized: "preparation", Type: models.DownloaderTypeFake, Enabled: true, MaxConcurrentDownloads: 2}
	if err := queue.db.Create(&provider).Error; err != nil {
		t.Fatal(err)
	}
	create := func(id, phase, status, kind string, held bool) models.DownloadTask {
		t.Helper()
		task := models.DownloadTask{ID: id, OwnerID: actor.User.ID, DownloaderID: &provider.ID, ProviderType: provider.Type, ProviderTag: "omc-" + id, DisplayName: id, Phase: phase, LastErrorCode: "downloader_unavailable", LastErrorMessage: downloadFailureMessage("downloader_unavailable", true)}
		source, err := json.Marshal(downloadSourceEnvelope{Kind: kind, URL: "https://115.com/s/original"})
		if err != nil {
			t.Fatal(err)
		}
		task.SourceCiphertext, err = downloads.credentials.Encrypt(downloadSourcePurpose(id), string(source))
		if err != nil {
			t.Fatal(err)
		}
		_, err = queue.EnqueueWith(EnqueueJobInput{OwnerID: actor.User.ID, JobType: "download", DisplayName: id, Payload: downloadJobPayload{DownloadTaskID: id}}, func(tx *gorm.DB, job models.Job) error {
			task.JobID = job.ID
			return tx.Create(&task).Error
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := queue.db.Model(&models.Job{}).Where("id = ?", task.JobID).Update("status", status).Error; err != nil {
			t.Fatal(err)
		}
		if held {
			if err := queue.db.Create(&models.DownloadAdmission{AcquisitionKey: downloadAcquisitionKey(task), DownloaderID: provider.ID, TaskID: id, State: "held", CreatedAt: clock.Now(), UpdatedAt: clock.Now()}).Error; err != nil {
				t.Fatal(err)
			}
		}
		return task
	}
	share := create("cancelled-share-preparation", models.DownloadTaskStatusSubmitting, models.JobStatusFailed, downloadpkg.SourcePan115Share, true)
	seed := create("cancelled-seed-preparation", models.DownloadTaskStatusSubmitting, models.JobStatusRetryWait, downloadpkg.SourceTorrent, true)
	fresh := create("new-distinct-download", models.DownloadTaskStatusQueued, models.JobStatusQueued, downloadpkg.SourceURL, false)
	for _, task := range []models.DownloadTask{share, seed} {
		if err := downloads.CancelPipeline(context.Background(), actor, task.ID, RequestContext{}); err != nil {
			t.Fatal(err)
		}
		if err := queue.db.Model(&models.Job{}).Where("id = ?", task.JobID).Updates(map[string]any{"status": models.JobStatusRetryWait, "next_attempt_at": clock.Now().Add(time.Minute)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if claim, err := queue.Claim([]string{"download"}); err != nil || claim != nil {
		t.Fatalf("old cancellation holds not reproduced: %+v %v", claim, err)
	}
	worker := NewDownloadWorker(downloads)
	if err := worker.RecoverOfflineStops(context.Background()); err != nil {
		t.Fatal(err)
	}
	claim, err := queue.Claim([]string{"download"})
	if err != nil || claim == nil || claim.Job.ID != fresh.JobID {
		t.Fatalf("cancelled preparation blocked new acquisition: %+v %v", claim, err)
	}
	if err := queue.RetryLater(claim.Job.ID, claim.LeaseToken, "", "", clock.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	clock.now = clock.Now().Add(2 * time.Minute)
	if err := queue.PromoteDueRetries(); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		claim, err = queue.Claim([]string{"download"})
		if err != nil || claim == nil {
			t.Fatalf("control claim=%+v %v", claim, err)
		}
		result := worker.Run(context.Background(), workerRuntime{queue: queue, job: *claim}, *claim)
		if result.RetryAt != nil || result.ErrorCode != "" {
			t.Fatalf("preparation kept looping: %+v", result)
		}
		if err := queue.Complete(claim.Job.ID, claim.LeaseToken); err != nil {
			t.Fatal(err)
		}
		var job models.Job
		if err := queue.db.First(&job, "id = ?", claim.Job.ID).Error; err != nil || job.Status != models.JobStatusCancelled || job.NextAttemptAt != nil {
			t.Fatalf("control did not terminate: %+v %v", job, err)
		}
	}
	if client.reconciles != 0 || client.submits != 0 || client.cancelled {
		t.Fatalf("preparation touched native task: reconciles=%d submits=%d cancel=%v", client.reconciles, client.submits, client.cancelled)
	}
	// A removed downloader config does not change the original share's lack of
	// native dispatch. Its cancellation must still reach a terminal state.
	orphan := create("share-with-removed-config", models.DownloadTaskStatusSubmitting, models.JobStatusFailed, downloadpkg.SourcePan115Share, false)
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", orphan.ID).Updates(map[string]any{"downloader_id": nil, "provider_type": models.DownloaderTypePan115Offline}).Error; err != nil {
		t.Fatal(err)
	}
	if err := downloads.CancelPipeline(context.Background(), actor, orphan.ID, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	claim, err = queue.Claim([]string{"download"})
	if err != nil || claim == nil || claim.Job.ID != orphan.JobID {
		t.Fatalf("removed-config control claim=%+v %v", claim, err)
	}
	result := worker.Run(context.Background(), workerRuntime{queue: queue, job: *claim}, *claim)
	if result.RetryAt != nil || result.ErrorCode != "" {
		t.Fatalf("removed config kept unsubmitted share pending: %+v", result)
	}
	if err := queue.Complete(claim.Job.ID, claim.LeaseToken); err != nil {
		t.Fatal(err)
	}
	cleanup := create("failed-owned-preparation", models.DownloadTaskStatusFailed, models.JobStatusFailed, downloadpkg.SourcePan115Share, true)
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", cleanup.ID).Update("last_error_code", "downloader_preparation_failed").Error; err != nil {
		t.Fatal(err)
	}
	if err := downloads.Delete(context.Background(), actor, cleanup.ID, true, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	// A repeated record-only request cannot silently clear a previously accepted
	// cleanup intent or claim that its files will be retained.
	if err := downloads.Delete(context.Background(), actor, cleanup.ID, false, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	var cleanupJob models.Job
	if err := queue.db.First(&cleanupJob, "id = ?", cleanup.JobID).Error; err != nil || !downloadControlDeletesData(cleanupJob.PayloadJSON) || strings.Contains(cleanupJob.LastErrorMessage, "源文件保留") {
		t.Fatalf("cleanup intent/message lost: %+v %v", cleanupJob, err)
	}
	if len(client.cleaned) != 0 {
		t.Fatal("HTTP deletion cleaned source outside a control lease")
	}
	claim, err = queue.Claim([]string{"download"})
	if err != nil || claim == nil || claim.Job.ID != cleanup.JobID {
		t.Fatalf("source cleanup was blocked by acquisition capacity: %+v %v", claim, err)
	}
	result = worker.Run(context.Background(), workerRuntime{queue: queue, job: *claim}, *claim)
	if result.RetryAt != nil || result.ErrorCode != "" {
		t.Fatalf("source cleanup failed: %+v", result)
	}
	if err := queue.Complete(claim.Job.ID, claim.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if len(client.cleaned) != 1 || client.cleaned[0] != cleanup.ProviderTag {
		t.Fatalf("source cleanup scope=%v", client.cleaned)
	}
	var stored models.DownloadTask
	if err := queue.db.First(&stored, "id = ?", cleanup.ID).Error; err != nil || stored.HistoryClearedAt == nil || stored.LastErrorCode != "" {
		t.Fatalf("cleanup receipt/display state=%+v %v", stored, err)
	}
}

func TestDownloadRetryExhaustionFinalizesDomainAndPreservesPreparationEvidence(t *testing.T) {
	downloads, _, queue, actor, _ := downloadFixture(t)
	clock := queue.clock.(*fakeQueueClock)
	if err := queue.db.Model(&models.QueuePolicy{}).Where("job_type = ?", "download").Update("max_attempts", 2).Error; err != nil {
		t.Fatal(err)
	}
	provider := models.Downloader{ID: "bounded-preparation-provider", OwnerID: actor.User.ID, Name: "Preparation", NameNormalized: "bounded-preparation", Type: models.DownloaderTypeFake, Enabled: true, MaxConcurrentDownloads: 2}
	if err := queue.db.Create(&provider).Error; err != nil {
		t.Fatal(err)
	}
	task := models.DownloadTask{ID: "bounded-preparation", OwnerID: actor.User.ID, DownloaderID: &provider.ID, ProviderType: provider.Type, DisplayName: "Bounded preparation", Phase: models.DownloadTaskStatusSubmitting, LastErrorCode: "downloader_preparation_failed"}
	_, err := queue.EnqueueWith(EnqueueJobInput{OwnerID: actor.User.ID, JobType: "download", DisplayName: task.DisplayName, Payload: downloadJobPayload{DownloadTaskID: task.ID}}, func(tx *gorm.DB, job models.Job) error {
		task.JobID = job.ID
		return tx.Create(&task).Error
	})
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		claim, err := queue.Claim([]string{"download"})
		if err != nil || claim == nil {
			t.Fatalf("claim=%+v %v", claim, err)
		}
		if err := queue.RetryLater(claim.Job.ID, claim.LeaseToken, "downloader_preparation_failed", downloadFailureMessage("downloader_preparation_failed", true), clock.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		clock.now = clock.Now().Add(time.Minute)
		if err := queue.PromoteDueRetries(); err != nil {
			t.Fatal(err)
		}
	}
	var job models.Job
	if err := queue.db.First(&job, "id = ?", task.JobID).Error; err != nil || job.Status != models.JobStatusFailed || job.FailureRetryCount != 2 || job.NextAttemptAt != nil {
		t.Fatalf("preparation retries not bounded: %+v %v", job, err)
	}
	if err := queue.db.First(&task, "id = ?", task.ID).Error; err != nil || task.Phase != models.DownloadTaskStatusFailed || task.FinishedAt == nil || task.LastErrorMessage != job.LastErrorMessage || task.OfflineStartedAt != nil || task.OfflineSettledAt != nil {
		t.Fatalf("terminal preparation projection=%+v %v", task, err)
	}
	if next, err := queue.Claim([]string{"download"}); err != nil || next != nil {
		t.Fatalf("failed preparation claimed again: %+v %v", next, err)
	}
	// Older failed Jobs with stale submitting snapshots have the same read model.
	task.Phase, task.LastErrorCode, task.LastErrorMessage = models.DownloadTaskStatusSubmitting, "downloader_unavailable", downloadFailureMessage("downloader_unavailable", true)
	if err := queue.db.Save(&task).Error; err != nil {
		t.Fatal(err)
	}
	rows, _, err := downloads.ListScoped(actor, DownloadListScopeActive, 10)
	if err != nil || len(rows) != 1 || rows[0].Phase != models.DownloadTaskStatusFailed || rows[0].AcquisitionState != "" {
		t.Fatalf("legacy failed projection=%+v %v", rows, err)
	}
	if summary := downloadTaskSummary(task, models.JobStatusFailed); summary.LastErrorMessage != downloadFailureMessage("downloader_unavailable", false) {
		t.Fatalf("legacy terminal message still promises retries: %q", summary.LastErrorMessage)
	}
}

func TestCloudOfflineDeadlineHistoryAndRequiredGoalLifecycle(t *testing.T) {
	downloads, downloaders, queue, actor, _ := downloadFixture(t)
	clock := queue.clock.(*fakeQueueClock)
	state := &offlineLifecycleFixtureState{clock: clock, starts: map[string]time.Time{}, ids: map[string]string{}}
	const providerType = models.DownloaderTypeFake
	if err := downloaders.registry.Register(providerType, downloadpkg.Capabilities{NativeOffline: true, Cancel: true}, func(config downloadpkg.Config) (downloadpkg.Client, error) {
		return &offlineLifecycleFixtureClient{state: state, namespace: config.OfflineAccountNamespace}, nil
	}); err != nil {
		t.Fatal(err)
	}
	connection := models.Connection{Name: "offline scope", NameNormalized: "offline-scope", Provider: "fixture-cloud", AccountID: "account-a", Enabled: true}
	if err := queue.db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	storage := models.Storage{Name: "offline source", NameNormalized: "offline-source", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), ConnectionID: &connection.ID, Enabled: true, Capabilities: "{}"}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	configureDownloadStaging(t, queue, storage.ID)
	downloader := models.Downloader{ID: "offline-fixture", OwnerID: actor.User.ID, Name: "cloud", NameNormalized: "cloud", Type: providerType, Enabled: true, StorageID: &storage.ID, ProviderDirectoryID: "owned", CapabilitiesJSON: `{"native_offline":true}`, MaxConcurrentDownloads: 2}
	if err := queue.db.Create(&downloader).Error; err != nil {
		t.Fatal(err)
	}
	plan := downloadpkg.OfflineRequiredPlan{Version: 1, RootName: "Feature", MultiFile: true, Files: []downloadpkg.OfflineRequiredFile{{RelativePath: "Feature.mkv", Size: 20 << 20}}}
	input := SubmitDownloadInput{DownloaderID: downloader.ID, DisplayName: "Feature", Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&tr=https://tracker-a.invalid", offlineRequired: &plan}}
	site := models.Site{Name: "Verified BT", NameNormalized: "verified-bt", Kind: "nyaa", BaseURL: "https://nyaa.si", Enabled: true, Revision: 1}
	if err := queue.db.Create(&site).Error; err != nil {
		t.Fatal(err)
	}
	actor.Permissions[authz.PermissionDiscoveryRead] = struct{}{}
	persistFixtureAuthority(t, queue.db, actor)
	publicIntake := SiteDownloadInput{ResultToken: strings.Repeat("r", 43), ContentMode: "all", DownloaderID: downloader.ID}
	input.SourceSiteID = site.ID
	input.offlineIntakeDigest = offlineSiteIntakeDigest(publicIntake)

	created, err := downloads.Submit(context.Background(), actor, input, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := queue.Claim([]string{"download"})
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v %v", claim, err)
	}
	worker := NewDownloadWorker(downloads)
	result := worker.Run(context.Background(), workerRuntime{queue: queue, job: *claim}, *claim)
	if result.RetryAt == nil || result.ErrorCode != "" {
		t.Fatalf("first observation=%+v", result)
	}
	var original models.DownloadTask
	if err := queue.db.First(&original, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if original.OfflineStartedAt == nil || original.OfflineDeadlineAt == nil || original.OfflineDeadlineAt.Sub(*original.OfflineStartedAt) != 30*time.Minute || original.OfflineStartProvenance != "dispatch" {
		t.Fatalf("clock=%+v", original)
	}
	start, deadline := *original.OfflineStartedAt, *original.OfflineDeadlineAt
	if err := queue.RetryLater(claim.Job.ID, claim.LeaseToken, "", "", *result.RetryAt); err != nil {
		t.Fatal(err)
	}
	clock.now = start.Add(12 * time.Minute)
	if err := queue.PromoteDueRetries(); err != nil {
		t.Fatal(err)
	}
	claim, err = queue.Claim([]string{"download"})
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	// A new worker instance continues the same operation, clock and failure budget.
	worker = NewDownloadWorker(downloads)
	result = worker.Run(context.Background(), workerRuntime{queue: queue, job: *claim}, *claim)
	if result.RetryAt == nil || result.ErrorCode != "" {
		t.Fatalf("continued observation=%+v", result)
	}
	if err := queue.RetryLater(claim.Job.ID, claim.LeaseToken, "", "", *result.RetryAt); err != nil {
		t.Fatal(err)
	}
	if err := queue.db.First(&original, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !original.OfflineStartedAt.Equal(start) || !original.OfflineDeadlineAt.Equal(deadline) || state.writes != 1 {
		t.Fatal("observation renewed or resubmitted original acquisition")
	}
	clock.now = deadline
	if err := worker.ExpireOfflineDeadlines(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := queue.db.First(&original, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if original.Phase != models.DownloadTaskStatusCancelled || original.LastErrorCode != CodeOfflineDeadlineExceeded || !original.OfflineStopPending || original.ProviderTaskID == "" {
		t.Fatalf("timeout=%+v", original)
	}
	var failures []models.OfflineFailure
	if err := queue.db.Find(&failures).Error; err != nil || len(failures) != 1 {
		t.Fatalf("history=%+v %v", failures, err)
	}
	// Clearing the display remains available while the physical stop is unknown.
	state.stopErr = errors.New("unconfirmed original stop")
	if err := downloads.Delete(context.Background(), actor, created.ID, false, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	claim, err = queue.Claim([]string{"download"})
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	result = worker.Run(context.Background(), workerRuntime{queue: queue, job: *claim}, *claim)
	if result.RetryAt == nil || result.ErrorCode != downloadCancellationPending {
		t.Fatalf("pending stop=%+v", result)
	}
	if err := queue.RetryLater(claim.Job.ID, claim.LeaseToken, result.ErrorCode, result.ErrorMessage, *result.RetryAt); err != nil {
		t.Fatal(err)
	}
	var admission models.DownloadAdmission
	if err := queue.db.First(&admission, "acquisition_key = ?", "task:"+created.ID).Error; err != nil || admission.State != "held" {
		t.Fatal("unknown stop freed acquisition")
	}
	state.stopErr = nil
	clock.now = clock.now.Add(2 * time.Minute)
	if err := queue.PromoteDueRetries(); err != nil {
		t.Fatal(err)
	}
	claim, err = queue.Claim([]string{"download"})
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	result = worker.Run(context.Background(), workerRuntime{queue: queue, job: *claim}, *claim)
	if result.ErrorCode != "" || result.RetryAt != nil {
		t.Fatalf("settled stop=%+v", result)
	}
	if err := queue.Complete(claim.Job.ID, claim.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if err := queue.db.First(&original, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if original.LastErrorCode != CodeOfflineDeadlineExceeded || original.OfflineStopPending || original.HistoryClearedAt == nil || state.writes != 1 {
		t.Fatalf("stop lost timeout or replayed write: %+v", original)
	}
	if err := downloads.deleteLocalRecord(original.ID, original.JobID, &actor.User.ID, "download.delete", "confirmed_keep_output", RequestContext{}); err != nil {
		t.Fatal(err)
	}
	// Reopened SQLite keeps policy history after physical display-row deletion.
	var dbFiles []struct{ File string }
	if err := queue.db.Raw("PRAGMA database_list").Scan(&dbFiles).Error; err != nil {
		t.Fatal(err)
	}
	reopened, err := database.Open(dbFiles[0].File)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := reopened.DB()
	defer sqlDB.Close()
	newQueue := NewQueueService(reopened, NewAuditService(reopened))
	newQueue.SetClock(clock)
	newDownloaders := NewDownloaderService(reopened, NewAuditService(reopened), downloads.credentials, downloaders.registry)
	restarted := NewDownloadService(reopened, NewAuditService(reopened), downloads.credentials, newDownloaders, NewDownloadSettingsService(reopened, NewAuditService(reopened)), newQueue, zerolog.Nop())
	// This scope has two older failures before the revision the user confirms.
	olderFailure := failures[0]
	olderFailure.ID, olderFailure.TaskID = 0, "older-offline-timeout"
	if err := reopened.Create(&olderFailure).Error; err != nil {
		t.Fatal(err)
	}
	confirmedFailure := olderFailure
	confirmedFailure.ID, confirmedFailure.TaskID = 0, "confirmed-offline-timeout"
	if err := reopened.Create(&confirmedFailure).Error; err != nil {
		t.Fatal(err)
	}
	input.Source.URL = "magnet:?xt=urn:btih:0123456789ABCDEF0123456789ABCDEF01234567&tr=https://different.invalid"
	_, err = restarted.Submit(context.Background(), actor, input, RequestContext{})
	var warning *AppError
	if !errors.As(err, &warning) || warning.Code != CodeOfflineConfirmationRequired || warning.OfflineFailureWarning == nil {
		t.Fatalf("missing scoped warning: %v", err)
	}
	token := warning.OfflineFailureWarning.ConfirmationToken
	changed := input
	changed.Priority = 1
	changed.OfflineFailureOverrideToken = token
	if _, err := restarted.Submit(context.Background(), actor, changed, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("changed request reused confirmation: %v", err)
	}
	automatic := input
	automatic.FollowSubscriptionID = "follow-fixture"
	automatic.FollowResourceFingerprint = strings.Repeat("a", 64)
	if _, err := restarted.Submit(context.Background(), actor, automatic, RequestContext{}); ErrorCode(err) != CodeOfflineConfirmationRequired {
		t.Fatalf("automatic intake bypassed history: %v", err)
	}
	input.OfflineFailureOverrideToken = token
	accepted, err := restarted.Submit(context.Background(), actor, input, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := restarted.Submit(context.Background(), actor, input, RequestContext{})
	if err != nil || replayed.ID != accepted.ID {
		t.Fatalf("confirmation replay=%+v %v", replayed, err)
	}
	// A consumed/missing Site claim is never restored for a lost HTTP reply.
	sites := &SiteService{db: reopened, downloads: restarted}
	publicIntake.OfflineFailureOverrideToken = token
	fromSite, err := sites.Download(context.Background(), actor, publicIntake, RequestContext{})
	if err != nil || fromSite.ID != accepted.ID {
		t.Fatalf("accepted Site replay=%+v %v", fromSite, err)
	}
	changedSite := publicIntake
	changedSite.ResultToken = strings.Repeat("s", 43)
	if _, err := sites.Download(context.Background(), actor, changedSite, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("changed Site request replayed=%v", err)
	}
	deny := models.UserAuthorizationRule{UserID: actor.User.ID, PermissionCode: authz.PermissionDiscoveryRead, Effect: models.AuthorizationEffectDeny, ResourceType: models.AuthorizationResourceSite, ResourceID: uintID(site.ID), CreatedBy: actor.User.ID}
	if err := reopened.Create(&deny).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := sites.Download(context.Background(), actor, publicIntake, RequestContext{}); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("revoked Site replay=%v", err)
	}
	if err := reopened.Delete(&deny).Error; err != nil {
		t.Fatal(err)
	}

	var jobs int64
	if err := reopened.Model(&models.DownloadTask{}).Where("id = ?", accepted.ID).Count(&jobs).Error; err != nil || jobs != 1 {
		t.Fatal("confirmation created duplicates")
	}
	// A later failure was not covered by that confirmation and must survive
	// this attempt's success, while all older same-scope audit rows are retained.
	newerFailure := confirmedFailure
	newerFailure.ID, newerFailure.TaskID = 0, "newer-offline-timeout"
	if err := reopened.Create(&newerFailure).Error; err != nil {
		t.Fatal(err)
	}
	// Required files can finish while extras remain. Their proof ends the timer
	// before recognition and preserves completed output plus a durable stop receipt.
	state.ready = false
	state.stopErr = errors.New("lost stop ack")
	claim, err = newQueue.Claim([]string{"download"})
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	result = NewDownloadWorker(restarted).Run(context.Background(), workerRuntime{queue: newQueue, job: *claim}, *claim)
	if result.RetryAt == nil || result.ErrorCode != "" {
		t.Fatalf("required observation=%+v", result)
	}
	if err := newQueue.RetryLater(claim.Job.ID, claim.LeaseToken, "", "", *result.RetryAt); err != nil {
		t.Fatal(err)
	}
	var due models.DownloadTask
	if err := reopened.First(&due, "id = ?", accepted.ID).Error; err != nil {
		t.Fatal(err)
	}
	if due.OfflineHistoryRevision != confirmedFailure.ID {
		t.Fatalf("confirmed history revision=%d, want %d", due.OfflineHistoryRevision, confirmedFailure.ID)
	}
	clock.now = *due.OfflineDeadlineAt
	state.ready = true
	originalProviderID := due.ProviderTaskID
	if err := reopened.Model(&due).Update("provider_task_id", "").Error; err != nil {
		t.Fatal(err)
	}
	if err := NewDownloadWorker(restarted).ExpireOfflineDeadlines(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := reopened.First(&due, "id = ?", accepted.ID).Error; err != nil || due.OfflineReadyAt == nil || due.Phase == models.DownloadTaskStatusCancelled || due.ProviderTaskID != originalProviderID {
		t.Fatalf("fresh ready lost to expiry: %+v %v", due, err)
	}

	state.stopErr = nil
	if err := NewDownloadWorker(restarted).RecoverOfflineStops(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !state.removed[due.ProviderTaskID] {
		t.Fatal("successful keep-output stop did not remove the fixture native task")
	}
	// Only the persisted required manifest remains; Get/Manifest now return not-found.
	if err := newQueue.PromoteDueRetries(); err != nil {
		t.Fatal(err)
	}
	claim, err = newQueue.Claim([]string{"download"})
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	result = NewDownloadWorker(restarted).Run(context.Background(), workerRuntime{queue: newQueue, job: *claim}, *claim)
	if result.ErrorCode != "" || result.RetryAt != nil {
		t.Fatalf("required goal=%+v", result)
	}
	var complete models.DownloadTask
	if err := reopened.First(&complete, "id = ?", accepted.ID).Error; err != nil {
		t.Fatal(err)
	}
	if complete.OfflineReadyAt == nil || complete.OfflineCompletionMode != downloadpkg.OfflineCompletionRequiredFiles || complete.OfflineStopPending || complete.Phase != models.DownloadTaskStatusCompleted {
		t.Fatalf("required readiness=%+v", complete)
	}
	for _, id := range []uint64{failures[0].ID, olderFailure.ID, confirmedFailure.ID} {
		var resolved models.OfflineFailure
		if err := reopened.First(&resolved, "id = ?", id).Error; err != nil || resolved.ResolvedAt == nil {
			t.Fatalf("successful retry did not retain and settle confirmed history through revision %d: %+v %v", confirmedFailure.ID, resolved, err)
		}
	}
	if err := reopened.First(&newerFailure, "id = ?", newerFailure.ID).Error; err != nil || newerFailure.ResolvedAt != nil {
		t.Fatalf("successful retry cleared a newer unconfirmed failure: %+v %v", newerFailure, err)
	}
	var remainingFailures []models.OfflineFailure
	if err := offlineFailureQuery(reopened, complete).Find(&remainingFailures).Error; err != nil || len(remainingFailures) != 1 || remainingFailures[0].ID != newerFailure.ID {
		t.Fatalf("old history resurfaced after success: %+v %v", remainingFailures, err)
	}
	if err := newQueue.Complete(claim.Job.ID, claim.LeaseToken); err != nil {
		t.Fatal(err)
	}
	clock.now = complete.OfflineDeadlineAt.Add(time.Hour)
	if err := NewDownloadWorker(restarted).ExpireOfflineDeadlines(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := reopened.First(&complete, "id = ?", accepted.ID).Error; err != nil || complete.LastErrorCode == CodeOfflineDeadlineExceeded {
		t.Fatal("later transport/stop wait renewed cloud timeout")
	}
	// Explicit no-task receipt and actual native terminal failure end maintenance
	// without fake ready or a later timeout history for those outcomes.
	for n := 0; n < 2; n++ {
		state.reject, state.terminal = n == 0, n == 1
		state.ready = false
		nextInput := input
		nextInput.OfflineFailureOverrideToken = ""
		nextInput.Source.URL = "magnet:?xt=urn:btih:" + strings.Repeat(map[int]string{0: "a", 1: "b"}[n], 40)
		nextInput.Source.offlineRequired = nil
		next, err := restarted.Submit(context.Background(), actor, nextInput, RequestContext{})
		if err != nil {
			t.Fatal(err)
		}
		nextClaim, err := newQueue.Claim([]string{"download"})
		if err != nil || nextClaim == nil {
			t.Fatal(err)
		}
		failed := NewDownloadWorker(restarted).Run(context.Background(), workerRuntime{queue: newQueue, job: *nextClaim}, *nextClaim)
		if failed.ErrorCode == "" {
			t.Fatalf("missing settled failure: %+v", failed)
		}
		if err := newQueue.Fail(nextClaim.Job.ID, nextClaim.LeaseToken, failed.ErrorCode, failed.ErrorMessage); err != nil {
			t.Fatal(err)
		}
		var rejected models.DownloadTask
		if err := reopened.First(&rejected, "id = ?", next.ID).Error; err != nil || rejected.OfflineSettledAt == nil || rejected.OfflineReadyAt != nil {
			t.Fatalf("terminal evidence=%+v %v", rejected, err)
		}
		clock.now = rejected.OfflineDeadlineAt.Add(time.Minute)
		if err := NewDownloadWorker(restarted).ExpireOfflineDeadlines(context.Background()); err != nil {
			t.Fatal(err)
		}
		var history int64
		if err := reopened.Model(&models.OfflineFailure{}).Where("task_id = ?", next.ID).Count(&history).Error; err != nil || history != 0 {
			t.Fatal("settled no-task/failed operation became timeout history")
		}
		if n == 0 {
			// Explicit user retry creates one distinct acquisition and preserves
			// the no-task receipt/clock, including Follow and Player backlinks.
			followID, fingerprint := "no-task-follow", strings.Repeat("e", 64)
			if err := reopened.Create(&models.FollowSubscription{ID: followID, OwnerID: actor.User.ID, MediaType: "tv", TMDBID: 999, Title: "Feature", Status: models.FollowStatusActive, Revision: 1, LifecycleRevision: 1, ExecutionSnapshotJSON: "{}"}).Error; err != nil {
				t.Fatal(err)
			}
			if err := reopened.Model(&rejected).Updates(map[string]any{"follow_subscription_id": followID, "follow_resource_fingerprint": fingerprint}).Error; err != nil {
				t.Fatal(err)
			}
			if err := reopened.Create(&models.FollowEpisodeClaim{SubscriptionID: followID, SeasonNumber: 1, EpisodeNumber: 2, State: "failed", ResourceFingerprint: fingerprint, UpdatedAt: clock.now}).Error; err != nil {
				t.Fatal(err)
			}
			acquisition := models.MediaAcquisition{ID: "no-task-acquisition", OwnerID: actor.User.ID, MediaType: "tv", TMDBID: 999, Stage: "download", Status: models.DownloadTaskStatusFailed, DownloadTaskID: rejected.ID, Revision: 1}
			if err := reopened.Create(&acquisition).Error; err != nil {
				t.Fatal(err)
			}
			denied := models.UserAuthorizationRule{UserID: actor.User.ID, PermissionCode: authz.PermissionDownloadsCreate, Effect: models.AuthorizationEffectDeny, ResourceType: models.AuthorizationResourceDownloader, ResourceID: downloader.ID, CreatedBy: actor.User.ID}
			if err := reopened.Create(&denied).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := newQueue.Control(actor, rejected.JobID, "retry", RequestContext{}); ErrorCode(err) != CodePermissionDenied {
				t.Fatalf("revoked no-task retry=%v", err)
			}
			if err := reopened.Delete(&denied).Error; err != nil {
				t.Fatal(err)
			}
			nextJob, err := newQueue.Control(actor, rejected.JobID, "retry", RequestContext{})
			if err != nil {
				t.Fatal(err)
			}
			var replacement models.DownloadTask
			if err := reopened.First(&replacement, "job_id = ?", nextJob.ID).Error; err != nil || replacement.ID == rejected.ID || replacement.OfflineStartedAt != nil || replacement.OfflineSettledAt != nil || replacement.ProviderTag == rejected.ProviderTag || replacement.SourceCiphertext == rejected.SourceCiphertext {
				t.Fatalf("no-task retry did not create distinct acquisition: %+v %v", replacement, err)
			}
			again, err := newQueue.Control(actor, rejected.JobID, "retry", RequestContext{})
			if err != nil || again.ID != nextJob.ID {
				t.Fatalf("no-task retry duplicated=%+v %v", again, err)
			}
			var old models.DownloadTask
			if err := reopened.First(&old, "id = ?", rejected.ID).Error; err != nil || old.OfflineSettledAt == nil || !old.OfflineStartedAt.Equal(*rejected.OfflineStartedAt) || old.ProviderTaskID != rejected.ProviderTaskID || old.Phase != models.DownloadTaskStatusFailed {
				t.Fatalf("original no-task receipt changed: %+v %v", old, err)
			}
			var claim models.FollowEpisodeClaim
			if err := reopened.First(&claim, "subscription_id = ?", followID).Error; err != nil || claim.DownloadTaskID == nil || *claim.DownloadTaskID != replacement.ID || claim.State != "downloading" {
				t.Fatalf("retry lost Follow ownership: %+v %v", claim, err)
			}
			if err := reopened.First(&acquisition, "id = ?", acquisition.ID).Error; err != nil || acquisition.DownloadTaskID != replacement.ID || acquisition.Status != models.DownloadTaskStatusQueued {
				t.Fatalf("retry lost acquisition projection: %+v %v", acquisition, err)
			}
			if err := restarted.CancelPipeline(context.Background(), actor, replacement.ID, RequestContext{}); err != nil {
				t.Fatal(err)
			}
		} else if _, err := newQueue.Control(actor, rejected.JobID, "retry", RequestContext{}); ErrorCode(err) != CodeQueueStateConflict {
			t.Fatalf("accepted terminal failure cloned a fresh clock: %v", err)
		}
	}

}

func TestPan115DownloadWaitBroadcastsLifeEventToAllWorkers(t *testing.T) {
	service := &DownloadService{providerEvents: newProviderEventWakeHub()}
	worker := &DownloadWorker{service: service, pan115PollInterval: time.Hour, heartbeatInterval: time.Hour}
	const connectionID = uint(115)
	generationA, _ := service.providerEvents.snapshot(connectionID)
	generationB := generationA
	doneA := make(chan error, 1)
	doneB := make(chan error, 1)
	go func() {
		doneA <- worker.waitForPan115Poll(context.Background(), &providerWakeRuntime{}, connectionID, &generationA, "task-a")
	}()
	go func() {
		doneB <- worker.waitForPan115Poll(context.Background(), &providerWakeRuntime{}, connectionID, &generationB, "task-b")
	}()

	if err := service.ProviderEventsChanged(context.Background(), connectionID, []models.ProviderEvent{{Kind: "created", ItemID: "output"}}); err != nil {
		t.Fatal(err)
	}
	for index, done := range []<-chan error{doneA, doneB} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("worker %d wake err=%v", index, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("worker %d was not woken by provider event", index)
		}
	}
	if generationA == 0 || generationB != generationA {
		t.Fatalf("event generations = %d, %d", generationA, generationB)
	}
}

func TestPan115DownloadWaitMaintainsQueueLeaseBetweenFallbackPolls(t *testing.T) {
	service := &DownloadService{providerEvents: newProviderEventWakeHub()}
	worker := &DownloadWorker{service: service, pan115PollInterval: 40 * time.Millisecond, heartbeatInterval: 5 * time.Millisecond}
	runtime := &providerWakeRuntime{heartbeats: make(chan struct{}, 1)}
	generation, _ := service.providerEvents.snapshot(115)
	done := make(chan error, 1)
	go func() { done <- worker.waitForPan115Poll(context.Background(), runtime, 115, &generation, "task-1") }()
	select {
	case <-runtime.heartbeats:
	case <-time.After(time.Second):
		t.Fatal("queue lease was not refreshed while waiting for fallback poll")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("fallback poll did not resume the worker")
	}
}

func TestDownloadSnapshotsSeedingPolicy(t *testing.T) {
	downloads, downloaders, queue, actor, _ := downloadFixture(t)
	root := t.TempDir()
	storage := models.Storage{Name: "Policy staging", NameNormalized: "policy staging", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	configureDownloadStaging(t, queue, storage.ID)
	if err := queue.db.Model(&models.SeedingSettings{}).Where("id = ?", 1).Updates(map[string]any{"enabled": true, "minimum_seed_minutes": 120, "minimum_ratio": 1.5, "completion_mode": models.SeedingCompletionAny}).Error; err != nil {
		t.Fatal(err)
	}
	provider, err := downloaders.Create(actor, DownloaderInput{Name: "Policy qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://127.0.0.1:8080", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:policy"}}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var task models.DownloadTask
	if err := queue.db.First(&task, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !task.SeedingCleanupEnabled || task.SeedingMinimumMinutes != 120 || task.SeedingMinimumRatio != 1.5 || task.SeedingCompletionMode != models.SeedingCompletionAny {
		t.Fatalf("seeding snapshot=%+v", task)
	}
}

func TestDownloadDisablesSeedingPolicyForNonSeedingProvider(t *testing.T) {
	downloads, downloaders, queue, actor, client := downloadFixture(t)
	if err := downloaders.registry.Register(models.DownloaderTypeFake, downloadpkg.Capabilities{Cancel: true}, func(downloadpkg.Config) (downloadpkg.Client, error) { return client, nil }); err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.SeedingSettings{}).Where("id = ?", 1).Updates(map[string]any{"enabled": true, "minimum_seed_minutes": 120, "minimum_ratio": 1.5, "completion_mode": models.SeedingCompletionAny}).Error; err != nil {
		t.Fatal(err)
	}
	provider, err := downloaders.Create(actor, DownloaderInput{Name: "Non-seeding provider", Type: models.DownloaderTypeFake, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:no-seeding"}}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var task models.DownloadTask
	if err := queue.db.First(&task, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if task.SeedingCleanupEnabled || task.SeedingMinimumMinutes == 120 || task.SeedingMinimumRatio == 1.5 || task.SeedingCompletionMode != models.SeedingCompletionAll {
		t.Fatalf("non-seeding provider retained seeding policy: %+v", task)
	}
}

func TestDownloadSubmitBatchKeepsPartialResultsIndexedAndPrivate(t *testing.T) {
	downloads, downloaders, _, actor, client := downloadFixture(t)
	if err := downloaders.registry.Register(models.DownloaderTypeFake, downloadpkg.Capabilities{Cancel: true}, func(downloadpkg.Config) (downloadpkg.Client, error) { return client, nil }); err != nil {
		t.Fatal(err)
	}
	provider, err := downloaders.Create(actor, DownloaderInput{Name: "Batch provider", Type: models.DownloaderTypeFake, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	private := "magnet:?xt=urn:btih:batch-private"
	result, err := downloads.SubmitBatch(context.Background(), actor, SubmitDownloadBatchInput{DownloaderID: provider.ID, SourceKind: downloadpkg.SourceURL, Sources: []string{private, "not-a-download-url", private}}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Submitted != 1 || result.Failed != 2 || len(result.Results) != 3 || result.Results[0].Task == nil || result.Results[1].ErrorCode == "" || result.Results[2].ErrorCode == "" {
		t.Fatalf("result=%+v", result)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), private) {
		t.Fatal("batch response leaked a private source URL")
	}
}

func configureDownloadStaging(t *testing.T, queue *QueueService, storageID uint) {
	t.Helper()
	var storage models.Storage
	if err := queue.db.First(&storage, storageID).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.DownloadSettings{}).Where("id = ?", 1).Updates(map[string]any{"absolute_path": storage.RootPath, "storage_id": nil, "relative_path": "/"}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestDownloadTargetRequiresExplicitLibraryAndKeepsSnapshot(t *testing.T) {
	downloads, downloaders, queue, actor, client := downloadFixture(t)
	createStorage := func(name, root string, enabled bool) models.Storage {
		storage := models.Storage{Name: name, NameNormalized: strings.ToLower(name), Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`}
		if err := queue.db.Create(&storage).Error; err != nil {
			t.Fatal(err)
		}
		if !enabled {
			if err := queue.db.Model(&storage).Update("enabled", false).Error; err != nil {
				t.Fatal(err)
			}
			storage.Enabled = false
		}
		return storage
	}
	staging := createStorage("Target staging", t.TempDir(), true)
	configureDownloadStaging(t, queue, staging.ID)
	provider, err := downloaders.Create(actor, DownloaderInput{Name: "Target qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := queue.db.Where("code = ?", "default-v1").First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	profile.RecognitionRulesJSON = `[{"enabled":true,"media_type":"all","pattern":"^Release\\.","replacement":""}]`
	profile.MovieDirectoryTemplate = "电影/{category}/{title} ({year})"
	profile.MovieFilenameTemplate = "{title} ({year})"
	profile.TVDirectoryTemplate = "剧集/{category}/{title} ({year})/Season {season:02}"
	profile.TVFilenameTemplate = "{title} - S{season:02}E{episode:02}"
	if err := queue.db.Save(&profile).Error; err != nil {
		t.Fatal(err)
	}

	unavailableStorage := createStorage("Unavailable target", t.TempDir(), false)
	availableStorage := createStorage("Available target", t.TempDir(), true)
	manualStorage := createStorage("Manual target", t.TempDir(), true)
	createLibrary := func(name string, storage models.Storage, order int, mode, conflict string) models.MediaLibrary {
		library := models.MediaLibrary{Name: name, NameNormalized: strings.ToLower(name), StorageID: storage.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision, RelativeRoot: "/", SortOrder: order, TransferMode: mode, ConflictPolicy: conflict, Enabled: true, Recursive: true, VideoExtensionsJSON: `[]`, IgnorePatternsJSON: `[]`, Status: models.MediaLibraryStatusListening}
		if err := queue.db.Create(&library).Error; err != nil {
			t.Fatal(err)
		}
		return library
	}
	createLibrary("Unavailable first", unavailableStorage, 1, models.MediaLibraryTransferMove, models.MediaLibraryConflictAsk)
	selected := createLibrary("Available second", availableStorage, 2, models.MediaLibraryTransferCopy, models.MediaLibraryConflictRename)
	manual := createLibrary("Manual third", manualStorage, 3, models.MediaLibraryTransferSymlink, models.MediaLibraryConflictSkip)

	automaticID := uint(0)
	if _, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, MediaLibraryID: &automaticID, Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:auto-target"}}, RequestContext{}); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("implicit target error=%v", err)
	}
	var taskCount, jobCount int64
	if err := queue.db.Model(&models.DownloadTask{}).Count(&taskCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.Job{}).Where("job_type = ?", "download").Count(&jobCount).Error; err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	submits := client.submits
	client.mu.Unlock()
	if taskCount != 0 || jobCount != 0 || submits != 0 {
		t.Fatalf("implicit target selected a library or submitted work: tasks=%d jobs=%d submits=%d", taskCount, jobCount, submits)
	}
	selectedID := selected.ID
	selectedTask, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, MediaLibraryID: &selectedID, Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:selected-target"}}, RequestContext{})
	if err != nil || selectedTask.TargetLibraryID == nil || *selectedTask.TargetLibraryID != selected.ID || selectedTask.ProfileID != profile.ID || selectedTask.TransferMode != models.MediaLibraryTransferCopy || selectedTask.ConflictPolicy != models.MediaLibraryConflictRename {
		t.Fatalf("selected target=%+v err=%v", selectedTask, err)
	}

	manualID := manual.ID
	explicit, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, MediaLibraryID: &manualID, Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:manual-target"}}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if explicit.TargetLibraryID == nil || *explicit.TargetLibraryID != manual.ID || explicit.TransferMode != models.MediaLibraryTransferSymlink {
		t.Fatalf("explicit target=%+v", explicit)
	}

	if err := queue.db.Model(&models.MediaLibrary{}).Where("id = ?", selected.ID).Updates(map[string]any{"name": "Changed later", "transfer_mode": models.MediaLibraryTransferMove, "conflict_policy": models.MediaLibraryConflictOverwrite}).Error; err != nil {
		t.Fatal(err)
	}
	var persisted models.DownloadTask
	if err := queue.db.First(&persisted, "id = ?", selectedTask.ID).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.TargetLibraryName != "Available second" || persisted.TransferMode != models.MediaLibraryTransferCopy || persisted.ConflictPolicy != models.MediaLibraryConflictRename || persisted.ProfileRevision != profile.Revision || persisted.ProfileBuiltinRecognitionPacksJSON != profile.BuiltinRecognitionPacksJSON || persisted.ProfileRecognitionRulesJSON != profile.RecognitionRulesJSON || persisted.MovieDirectoryTemplate != normalizeMediaTypeDirectoryTemplate(profile.MovieDirectoryTemplate, "movie") || persisted.TVDirectoryTemplate != normalizeMediaTypeDirectoryTemplate(profile.TVDirectoryTemplate, "tv") || persisted.TransferRouteKind != models.TransferRouteSameSourceLocal || persisted.TransferRouteVersion != models.TransferRouteVersionCurrent || persisted.SourceDataSourceJSON == "{}" || persisted.TargetDataSourceJSON == "{}" {
		t.Fatalf("download target snapshot changed with library: %+v", persisted)
	}
}

func TestPan115DownloadTargetRequiresAvailableCrossSourceCapabilitiesAndWritableMode(t *testing.T) {
	downloads, downloaders, queue, actor, _ := downloadFixture(t)
	now := time.Now().UTC()
	connectionA := models.Connection{Name: "115 A", NameNormalized: "115-a-target", Provider: cloudpkg.ProviderPan115, CredentialCiphertext: "encrypted", Enabled: true, Revision: 1, CreatedAt: now, UpdatedAt: now}
	connectionB := models.Connection{Name: "115 B", NameNormalized: "115-b-target", Provider: cloudpkg.ProviderPan115, CredentialCiphertext: "encrypted", Enabled: true, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := queue.db.Create(&connectionA).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Create(&connectionB).Error; err != nil {
		t.Fatal(err)
	}
	driver := newFakeMutationCloudDriver()
	for _, item := range []cloudpkg.Item{{ID: "0", IsDir: true}, {ID: "storage-a", ParentID: "0", Name: "a", IsDir: true}, {ID: "library-a", ParentID: "storage-a", Name: "library", IsDir: true}} {
		driver.items[item.ID] = item
	}
	downloaders.connections = &ConnectionService{db: queue.db, drivers: map[uint]cloudpkg.Driver{connectionA.ID: driver, connectionB.ID: newFakeMutationCloudDriver()}}
	sourceStorage := models.Storage{Name: "Source A", NameNormalized: "source-a-target", Type: models.StorageTypePan115, RootPath: "storage-a", RootPathNormalized: "pan115:storage-a", ConnectionID: &connectionA.ID, Enabled: true, Capabilities: `{}`}
	targetStorage := models.Storage{Name: "Target A", NameNormalized: "target-a-target", Type: models.StorageTypePan115, RootPath: "storage-a", RootPathNormalized: "pan115:target-a", ConnectionID: &connectionA.ID, Enabled: true, Capabilities: `{}`}
	if err := queue.db.Create(&sourceStorage).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Create(&targetStorage).Error; err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := queue.db.Where("code = ?", "default-v1").First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	library := models.MediaLibrary{Name: "115 Movies", NameNormalized: "115-movies-target", StorageID: targetStorage.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision, RelativeRoot: "/library", ProviderRootID: "library-a", SortOrder: 1, TransferMode: models.MediaLibraryTransferCopy, ConflictPolicy: models.MediaLibraryConflictAsk, Enabled: true, Recursive: true, VideoExtensionsJSON: `[]`, IgnorePatternsJSON: `[]`}
	if err := queue.db.Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	provider := models.Downloader{ID: "pan115-target", Name: "115 Offline", NameNormalized: "115-offline-target", Type: models.DownloaderTypePan115Offline, StorageID: &sourceStorage.ID, Enabled: true, CapabilitiesJSON: `{}`}
	if err := queue.db.Model(&library).Update("default_ingest_connection_id", connectionA.ID).Error; err != nil {
		t.Fatal(err)
	}
	if summary := downloaders.summary(actor, provider); summary.LifeEventDefaultLibraryID == nil || *summary.LifeEventDefaultLibraryID != library.ID {
		t.Fatalf("default library omitted for allowed actor: %+v", summary)
	}
	restricted := actor
	restricted.ResourceAccessPolicies = map[string]ResourceAccessPolicy{models.ResourceAccessScopeLibraryRead: {Mode: models.ResourceAccessModeAllowlist}}
	if summary := downloaders.summary(restricted, provider); summary.LifeEventDefaultLibraryID != nil || summary.LifeEventDefaultLibraryName != "" {
		t.Fatalf("downloader disclosed forbidden default library: %+v", summary)
	}
	localRoot := t.TempDir()
	localStorage := models.Storage{Name: "Local Target", NameNormalized: "local-target-" + strings.ToLower(filepath.Base(localRoot)), Type: models.StorageTypeLocal, RootPath: localRoot, RootPathNormalized: strings.ToLower(localRoot), Enabled: true, Capabilities: `{}`}
	if err := queue.db.Create(&localStorage).Error; err != nil {
		t.Fatal(err)
	}
	localLibrary := models.MediaLibrary{Name: "Local First", NameNormalized: "local-first-" + strings.ToLower(filepath.Base(localRoot)), StorageID: localStorage.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision, RelativeRoot: "/", SortOrder: 0, TransferMode: models.MediaLibraryTransferMove, ConflictPolicy: models.MediaLibraryConflictAsk, Enabled: true, Recursive: true, VideoExtensionsJSON: `[]`, IgnorePatternsJSON: `[]`}
	if err := queue.db.Create(&localLibrary).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := downloads.snapshotDownloadTarget(context.Background(), provider, localLibrary); ErrorCode(err) != CodeTransferRouteUnsupported {
		t.Fatalf("pan115 local-target error=%v", err)
	}
	if automatic, _, err := downloads.resolveDownloadTarget(context.Background(), provider, 0, downloadpkg.SourceURL); ErrorCode(err) != CodeMediaLibraryStorageUnavailable {
		t.Fatalf("implicit target selection survived: target=%+v err=%v", automatic, err)
	}
	target, _, err := downloads.snapshotDownloadTarget(context.Background(), provider, library)
	if err != nil || target.ConnectionID == nil || *target.ConnectionID != connectionA.ID || target.ProviderRootID != "library-a" || target.StorageType != models.StorageTypePan115 {
		t.Fatalf("target=%+v err=%v", target, err)
	}

	library.TransferMode = models.MediaLibraryTransferSymlink
	if _, _, err := downloads.snapshotDownloadTarget(context.Background(), provider, library); ErrorCode(err) != CodeTransferRouteUnsupported {
		t.Fatalf("symlink error=%v", err)
	}
	library.TransferMode = models.MediaLibraryTransferMove
	targetStorage.ConnectionID = &connectionB.ID
	if err := queue.db.Model(&targetStorage).Update("connection_id", connectionB.ID).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := downloads.snapshotDownloadTarget(context.Background(), provider, library); ErrorCode(err) != CodeTransferRouteUnsupported {
		t.Fatalf("cross-connection error=%v", err)
	}
	qbit := provider
	qbit.Type = models.DownloaderTypeQBittorrent
	if _, _, err := downloads.snapshotDownloadTarget(context.Background(), qbit, library); ErrorCode(err) != CodeTransferRouteUnsupported {
		t.Fatalf("qBittorrent cloud-target error=%v", err)
	}
}

func TestDownloaderCredentialsAndDownloadSourceStayEncrypted(t *testing.T) {
	downloads, downloaders, queue, actor, _ := downloadFixture(t)
	root := t.TempDir()
	storage := models.Storage{Name: "Staging", NameNormalized: "staging", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	configureDownloadStaging(t, queue, storage.ID)
	created, err := downloaders.Create(actor, DownloaderInput{Name: "qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Username: "admin", Password: "super-secret", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !created.UsernameConfigured || !created.PasswordConfigured {
		t.Fatal("credential flags missing")
	}
	var rawDownloader models.Downloader
	if err := queue.db.First(&rawDownloader, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if rawDownloader.UsernameCiphertext == "admin" || rawDownloader.PasswordCiphertext == "super-secret" || strings.Contains(rawDownloader.PasswordCiphertext, "super-secret") {
		t.Fatal("credentials persisted in plaintext")
	}

	other, err := downloaders.Create(actor, DownloaderInput{Name: "Other receiver", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://other.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := downloaders.Create(actor, DownloaderInput{Name: "Disabled receiver", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://disabled.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.Downloader{}).Where("id = ?", disabled.ID).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.Downloader{}).Where("id = ?", created.ID).Update("sort_order", 20).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.Downloader{}).Where("id = ?", other.ID).Update("sort_order", 10).Error; err != nil {
		t.Fatal(err)
	}
	ordinary := rolePermissionActor([]string{authz.PermissionDownloadsCreate})
	ordinary.User = actor.User
	if _, err := downloaders.List(ordinary); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("use account read management: %v", err)
	}
	options, err := downloaders.Options(context.Background(), ordinary)
	if err != nil || len(options) != 2 || options[0].ID != other.ID || options[1].ID != created.ID {
		t.Fatalf("ordered enabled use options=%+v err=%v", options, err)
	}
	encoded, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	var safeOptions []map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &safeOptions); err != nil {
		t.Fatal(err)
	}
	for _, option := range safeOptions {
		if len(option) != 5 || option["id"] == nil || option["name"] == nil || option["type"] == nil || option["capabilities"] == nil || option["execution_location"] == nil {
			t.Fatalf("unsafe use option fields: %s", encoded)
		}
	}
	if strings.Contains(string(encoded), "example.test") || strings.Contains(string(encoded), "super-secret") || strings.Contains(string(encoded), root) {
		t.Fatal("use options leaked connection/source configuration")
	}

	item, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: created.ID, DisplayName: "安全测试", Priority: 10, Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "https://tracker.example.test/file?id=1&passkey=pt-secret"}}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var task models.DownloadTask
	if err := queue.db.First(&task, "id = ?", item.ID).Error; err != nil {
		t.Fatal(err)
	}
	if references, err := downloads.settings.StorageReferences(storage.ID); err != nil || len(references) != 0 {
		t.Fatalf("staging references=%v err=%v", references, err)
	}
	if strings.Contains(task.SourceCiphertext, "pt-secret") {
		t.Fatal("download source persisted in plaintext")
	}
	if task.ProfileID == 0 || task.ProfileRevision != 1 || task.ProfileRulesJSON == "" || task.ProfileBuiltinRecognitionPacksJSON == "" || task.ProfileRecognitionRulesJSON == "" {
		t.Fatalf("classification profile was not snapshotted: %+v", task)
	}
	var job models.Job
	if err := queue.db.First(&job, "id = ?", item.JobID).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(job.PayloadJSON, "tracker") || strings.Contains(job.PayloadJSON, "pt-secret") {
		t.Fatal("source leaked into job payload")
	}
	var auditRows []models.AuditLog
	if err := queue.db.Order("id").Find(&auditRows).Error; err != nil {
		t.Fatal(err)
	}
	for _, auditRow := range auditRows {
		if strings.Contains(auditRow.Metadata, "pt-secret") || strings.Contains(auditRow.Metadata, "super-secret") || strings.Contains(auditRow.Metadata, root) || strings.Contains(auditRow.Metadata, "tracker.example") {
			t.Fatalf("audit metadata leaked sensitive download state: %s", auditRow.Metadata)
		}
	}
	var payload downloadJobPayload
	if err := json.Unmarshal([]byte(job.PayloadJSON), &payload); err != nil || payload.DownloadTaskID != item.ID {
		t.Fatalf("payload=%+v err=%v", payload, err)
	}
	listed, err := downloads.List(actor, 10)
	if err != nil || len(listed) != 1 || listed[0].DisplayName != "安全测试" {
		t.Fatalf("list=%+v err=%v", listed, err)
	}
	if task.StagingAbsolutePath != root || task.StagingStorageID != nil {
		t.Fatalf("new task did not snapshot independent staging path: %+v", task)
	}
	if err := queue.db.Where("user_id = ? AND permission_code = ?", actor.User.ID, authz.PermissionDownloadsCreate).Delete(&models.UserAuthorizationRule{}).Error; err != nil {
		t.Fatal(err)
	}
	exactRule := models.UserAuthorizationRule{UserID: actor.User.ID, PermissionCode: authz.PermissionDownloadsCreate, Effect: models.AuthorizationEffectAllow, ResourceType: models.AuthorizationResourceDownloader, ResourceID: created.ID, CreatedBy: actor.User.ID}
	if err := queue.db.Create(&exactRule).Error; err != nil {
		t.Fatal(err)
	}
	exact, err := NewAuthorizationService(queue.db).Resolve(actor.User.ID)
	if err != nil || exact.Can(authz.PermissionDownloadsCreate) {
		t.Fatalf("exact-only authority=%+v err=%v", exact, err)
	}
	options, err = downloaders.Options(context.Background(), exact)
	if err != nil || len(options) != 1 || options[0].ID != created.ID {
		t.Fatalf("exact-only options=%+v err=%v", options, err)
	}
	if _, err := downloads.PreviewRoutes(context.Background(), exact, DownloadRoutePreviewInput{DownloaderID: created.ID, SourceKind: "url"}); err != nil {
		t.Fatalf("exact-only preview=%v", err)
	}
	input := SubmitDownloadInput{DownloaderID: created.ID, Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "https://source.example.test/exact"}}
	if _, err := downloads.Submit(context.Background(), exact, input, RequestContext{}); err != nil {
		t.Fatalf("exact-only submission=%v", err)
	}
	input.DownloaderID = other.ID
	if _, err := downloads.Submit(context.Background(), exact, input, RequestContext{}); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("exact grant admitted another downloader: %v", err)
	}
	if _, err := downloads.PreviewRoutes(context.Background(), exact, DownloadRoutePreviewInput{DownloaderID: other.ID, SourceKind: "url"}); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("exact grant previewed another downloader: %v", err)
	}
	exact.ResourceRules = append(exact.ResourceRules, AuthorizationRule{PermissionCode: authz.PermissionDownloadsCreate, Effect: models.AuthorizationEffectDeny, ResourceType: models.AuthorizationResourceDownloader, ResourceID: created.ID})
	if options, err := downloaders.Options(context.Background(), exact); err != nil || len(options) != 0 {
		t.Fatalf("resource deny options=%+v err=%v", options, err)
	}
	exact.DeniedPermissions = map[string]struct{}{authz.PermissionDownloadsCreate: {}}
	if _, err := downloaders.Options(context.Background(), exact); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("global deny options=%v", err)
	}
}

func TestFollowDownloadSubmissionIsIdempotentAndRetriesTerminalTask(t *testing.T) {
	downloads, downloaders, queue, actor, _ := downloadFixture(t)
	root := t.TempDir()
	storage := models.Storage{Name: "Follow staging", NameNormalized: "follow-staging", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	configureDownloadStaging(t, queue, storage.ID)
	provider, err := downloaders.Create(actor, DownloaderInput{Name: "Follow qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://follow-qbit.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	input := SubmitDownloadInput{
		DownloaderID:              provider.ID,
		DisplayName:               "Fixture Show S01E01",
		Source:                    DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:follow-idempotent"},
		FollowSubscriptionID:      "follow-subscription",
		FollowResourceFingerprint: strings.Repeat("a", 64),
	}
	first, err := downloads.Submit(context.Background(), actor, input, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := downloads.Submit(context.Background(), actor, input, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.ID != first.ID {
		t.Fatalf("active duplicate created another task: first=%s duplicate=%s", first.ID, duplicate.ID)
	}
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", first.ID).Update("phase", models.DownloadTaskStatusFailed).Error; err != nil {
		t.Fatal(err)
	}
	retry, err := downloads.Submit(context.Background(), actor, input, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if retry.ID == first.ID {
		t.Fatal("failed follow download was not replaced on retry")
	}
	var old models.DownloadTask
	if err := queue.db.First(&old, "id = ?", first.ID).Error; err != nil {
		t.Fatal(err)
	}
	if old.FollowSubscriptionID != "" || old.FollowResourceFingerprint != "" {
		t.Fatalf("terminal task retained follow idempotency claim: %+v", old)
	}
	var active int64
	if err := queue.db.Model(&models.DownloadTask{}).Where("follow_subscription_id = ? AND follow_resource_fingerprint = ?", input.FollowSubscriptionID, input.FollowResourceFingerprint).Count(&active).Error; err != nil || active != 1 {
		t.Fatalf("active follow download count=%d err=%v", active, err)
	}
}

func TestFollowDownloadBeforePersistRollsBackJobAndTask(t *testing.T) {
	downloads, downloaders, queue, actor, _ := downloadFixture(t)
	root := t.TempDir()
	storage := models.Storage{Name: "Follow guarded staging", NameNormalized: "follow-guarded-staging", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	configureDownloadStaging(t, queue, storage.ID)
	provider, err := downloaders.Create(actor, DownloaderInput{Name: "Guarded qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://guarded-qbit.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var jobsBefore, tasksBefore int64
	if err := queue.db.Model(&models.Job{}).Count(&jobsBefore).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.DownloadTask{}).Count(&tasksBefore).Error; err != nil {
		t.Fatal(err)
	}
	guardCalled := false
	_, err = downloads.Submit(context.Background(), actor, SubmitDownloadInput{
		DownloaderID:              provider.ID,
		DisplayName:               "Fixture Show S01E01",
		Source:                    DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:follow-guarded"},
		FollowSubscriptionID:      "follow-guarded",
		FollowResourceFingerprint: strings.Repeat("b", 64),
		BeforePersist: func(tx *gorm.DB) error {
			guardCalled = tx != nil
			return appError(CodeConflict, "订阅已暂停", nil)
		},
	}, RequestContext{})
	if ErrorCode(err) != CodeConflict || !guardCalled {
		t.Fatalf("guard err=%v called=%v", err, guardCalled)
	}
	var jobsAfter, tasksAfter int64
	if err := queue.db.Model(&models.Job{}).Count(&jobsAfter).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.DownloadTask{}).Count(&tasksAfter).Error; err != nil {
		t.Fatal(err)
	}
	if jobsAfter != jobsBefore || tasksAfter != tasksBefore {
		t.Fatalf("guard rejection left partial state: jobs %d->%d tasks %d->%d", jobsBefore, jobsAfter, tasksBefore, tasksAfter)
	}
}

func TestDeleteTerminalDownloadRemovesProviderDataAndAllLocalQueueFacts(t *testing.T) {
	downloads, downloaders, queue, actor, client := downloadFixture(t)
	actor.Permissions[authz.PermissionJobsControlOwn] = struct{}{}
	root := t.TempDir()
	storage := models.Storage{Name: "Delete staging", NameNormalized: "delete staging", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	configureDownloadStaging(t, queue, storage.ID)
	provider, err := downloaders.Create(actor, DownloaderInput{Name: "Delete qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:delete"}}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", created.ID).Updates(map[string]any{"provider_task_id": "provider-hash", "phase": models.DownloadTaskStatusFailed}).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.Job{}).Where("id = ?", created.JobID).Updates(map[string]any{"status": models.JobStatusFailed, "last_error_code": "old", "last_error_message": "old failure"}).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := queue.db.Create(&models.JobActionRequest{JobID: created.JobID, Version: 1, ActionType: "legacy", Prompt: "legacy", OptionsJSON: `[]`, CreatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := downloads.Delete(context.Background(), actor, created.ID, true, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	cancelled, deleteData := client.cancelled, client.deleteData
	client.mu.Unlock()
	if !cancelled || !deleteData {
		t.Fatalf("provider cleanup cancelled=%v deleteData=%v", cancelled, deleteData)
	}
	checks := []struct {
		model any
		where string
		value string
	}{
		{model: &models.DownloadTask{}, where: "id = ?", value: created.ID},
		{model: &models.Job{}, where: "id = ?", value: created.JobID},
		{model: &models.JobActionRequest{}, where: "job_id = ?", value: created.JobID},
		{model: &models.JobAttempt{}, where: "job_id = ?", value: created.JobID},
		{model: &models.JobStatusEvent{}, where: "job_id = ?", value: created.JobID},
	}
	for _, check := range checks {
		var count int64
		if err := queue.db.Model(check.model).Where(check.where, check.value).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("orphan model %T count=%d err=%v", check.model, count, err)
		}
	}
	var audit models.AuditLog
	if err := queue.db.Where("action = ? AND target_id = ?", "download.delete", created.ID).First(&audit).Error; err != nil || strings.Contains(audit.Metadata, root) {
		t.Fatalf("audit=%+v err=%v", audit, err)
	}
}

func TestDeleteTerminalDownloadDefaultsToRemovingProviderTaskAndRetainingData(t *testing.T) {
	downloads, downloaders, queue, actor, client := downloadFixture(t)
	actor.Permissions[authz.PermissionJobsControlOwn] = struct{}{}
	root := t.TempDir()
	storage := models.Storage{Name: "Default delete staging", NameNormalized: "default delete staging", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	configureDownloadStaging(t, queue, storage.ID)
	provider, err := downloaders.Create(actor, DownloaderInput{Name: "Default delete qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:default-delete"}}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", created.ID).Updates(map[string]any{"provider_task_id": "provider-hash", "phase": models.DownloadTaskStatusFailed}).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.Job{}).Where("id = ?", created.JobID).Update("status", models.JobStatusFailed).Error; err != nil {
		t.Fatal(err)
	}
	if err := downloads.Delete(context.Background(), actor, created.ID, false, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	cancelled, deleteData := client.cancelled, client.deleteData
	client.mu.Unlock()
	if !cancelled || deleteData {
		t.Fatalf("provider cleanup cancelled=%v deleteData=%v", cancelled, deleteData)
	}
	var count int64
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", created.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("local record count=%d err=%v", count, err)
	}
}

func TestDeleteTerminalDownloadWithoutProviderIdentityIsSafeByDefaultAndDestructiveModeFailsClosed(t *testing.T) {
	downloads, downloaders, queue, actor, _ := downloadFixture(t)
	actor.Permissions[authz.PermissionJobsControlOwn] = struct{}{}
	root := t.TempDir()
	storage := models.Storage{Name: "Unsubmitted delete staging", NameNormalized: "unsubmitted delete staging", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	configureDownloadStaging(t, queue, storage.ID)
	provider, err := downloaders.Create(actor, DownloaderInput{Name: "Unsubmitted delete qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	createFailed := func(name, hash string) DownloadTaskSummary {
		created, submitErr := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, DisplayName: name, Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:" + hash}}, RequestContext{})
		if submitErr != nil {
			t.Fatal(submitErr)
		}
		if updateErr := queue.db.Model(&models.DownloadTask{}).Where("id = ?", created.ID).Update("phase", models.DownloadTaskStatusFailed).Error; updateErr != nil {
			t.Fatal(updateErr)
		}
		if updateErr := queue.db.Model(&models.Job{}).Where("id = ?", created.JobID).Update("status", models.JobStatusFailed).Error; updateErr != nil {
			t.Fatal(updateErr)
		}
		return created
	}

	destructive := createFailed("Unsubmitted destructive", "unsubmitted-destructive")
	if err := downloads.Delete(context.Background(), actor, destructive.ID, true, RequestContext{}); ErrorCode(err) != CodeDownloaderUnavailable {
		t.Fatalf("destructive delete without provider identity error=%v", err)
	}
	var count int64
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", destructive.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("destructive local record count=%d err=%v", count, err)
	}

	ordinary := createFailed("Unsubmitted ordinary", "unsubmitted-ordinary")
	if err := downloads.Delete(context.Background(), actor, ordinary.ID, false, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", ordinary.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("ordinary local record count=%d err=%v", count, err)
	}
}

func TestDeleteTerminalDownloadHidesRecordAndRetainsPrivateCleanupWhenProviderUnavailable(t *testing.T) {
	downloads, downloaders, queue, actor, client := downloadFixture(t)
	actor.Permissions[authz.PermissionJobsControlOwn] = struct{}{}
	root := t.TempDir()
	storage := models.Storage{Name: "Retain staging", NameNormalized: "retain staging", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	configureDownloadStaging(t, queue, storage.ID)
	provider, err := downloaders.Create(actor, DownloaderInput{Name: "Retain qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:retain"}}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", created.ID).Updates(map[string]any{"provider_task_id": "provider-hash", "phase": models.DownloadTaskStatusFailed}).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.Job{}).Where("id = ?", created.JobID).Update("status", models.JobStatusFailed).Error; err != nil {
		t.Fatal(err)
	}
	client.cancelErr = downloadpkg.Error("downloader_unavailable", true, errors.New("offline"))
	if err := downloads.Delete(context.Background(), actor, created.ID, false, RequestContext{}); err != nil {
		t.Fatalf("provider outage blocked display removal: %v", err)
	}
	visible, total, err := downloads.ListScoped(actor, DownloadListScopeAll, 20)
	if err != nil || total != 0 || len(visible) != 0 {
		t.Fatalf("removed record visible: %+v %v", visible, err)
	}
	var count int64
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", created.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("local record count=%d err=%v", count, err)
	}
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", created.ID).Update("downloader_id", nil).Error; err != nil {
		t.Fatal(err)
	}
	if err := downloads.Delete(context.Background(), actor, created.ID, false, RequestContext{}); err != nil {
		t.Fatalf("repeat removal required a reachable downloader: %v", err)
	}
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", created.ID).Update("downloader_id", provider.ID).Error; err != nil {
		t.Fatal(err)
	}
	client.cancelErr = downloadpkg.Error("downloader_task_not_found", false, errors.New("missing"))
	if err := downloads.Delete(context.Background(), actor, created.ID, false, RequestContext{}); err != nil {
		t.Fatalf("manual provider deletion was not idempotent: %v", err)
	}
}

func TestDeleteCompletedDownloadPreflightsUnfinishedTransferBeforeProviderCleanup(t *testing.T) {
	downloads, downloaders, queue, actor, client := downloadFixture(t)
	actor.Permissions[authz.PermissionJobsControlOwn] = struct{}{}
	root := t.TempDir()
	storage := models.Storage{Name: "Delete composite staging", NameNormalized: "delete composite staging", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	configureDownloadStaging(t, queue, storage.ID)
	provider, err := downloaders.Create(actor, DownloaderInput{Name: "Delete composite qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:delete-composite"}}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", created.ID).Updates(map[string]any{"provider_task_id": "provider-must-remain", "phase": models.DownloadTaskStatusCompleted, "finished_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.Job{}).Where("id = ?", created.JobID).Updates(map[string]any{"status": models.JobStatusCompleted, "finished_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	transferJob, err := queue.Enqueue(EnqueueJobInput{OwnerID: actor.User.ID, JobType: "transfer", DisplayName: "Unfinished transfer", Payload: map[string]any{"transfer_task_id": "delete-composite-transfer"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.Job{}).Where("id = ?", transferJob.ID).Updates(map[string]any{"status": models.JobStatusFailed, "finished_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	transfer := models.TransferTask{ID: "delete-composite-transfer", OwnerID: actor.User.ID, JobID: transferJob.ID, DownloadTaskID: created.ID, LibraryID: 1, LibraryName: "Target", ManifestJSON: `{}`, SourceManifestJSON: `{}`, Phase: models.TransferTaskStatusFailed, CreatedAt: now, UpdatedAt: now, FinishedAt: &now}
	if err := queue.db.Create(&transfer).Error; err != nil {
		t.Fatal(err)
	}

	if err := downloads.Delete(context.Background(), actor, created.ID, false, RequestContext{}); ErrorCode(err) != CodeQueueStateConflict {
		t.Fatalf("unfinished composite delete err=%v", err)
	}
	client.mu.Lock()
	cancelCalls := len(client.cancelIDs)
	client.mu.Unlock()
	if cancelCalls != 0 {
		t.Fatalf("provider was touched before composite preflight: cancel calls=%d", cancelCalls)
	}
	var count int64
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", created.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("local task count=%d err=%v", count, err)
	}
}

func TestDownloaderRejectsAmbiguousCredentialClearAndOversizedConfig(t *testing.T) {
	_, downloaders, _, actor, _ := downloadFixture(t)
	for _, baseURL := range []string{"http://qbit.example.test/?", "http://qbit.example.test/#"} {
		if _, err := downloaders.Create(actor, DownloaderInput{Name: "Unsafe " + baseURL, Type: models.DownloaderTypeQBittorrent, BaseURL: baseURL, Enabled: true}, RequestContext{}); ErrorCode(err) != CodeInvalidRequest {
			t.Fatalf("unsafe base URL %q error=%v", baseURL, err)
		}
	}
	created, err := downloaders.Create(actor, DownloaderInput{Name: "qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Username: "admin", Password: "secret", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if created.MaxConcurrentDownloads != 2 {
		t.Fatalf("default concurrency=%d", created.MaxConcurrentDownloads)
	}
	for _, limit := range []int{0, -1, 65} {
		if _, err := downloaders.Update(actor, created.ID, UpdateDownloaderInput{MaxConcurrentDownloads: &limit}, RequestContext{}); ErrorCode(err) != CodeInvalidRequest {
			t.Fatalf("invalid limit %d=%v", limit, err)
		}
	}
	limit := 64
	if changed, err := downloaders.Update(actor, created.ID, UpdateDownloaderInput{MaxConcurrentDownloads: &limit}, RequestContext{}); err != nil || changed.MaxConcurrentDownloads != 64 {
		t.Fatalf("updated concurrency=%+v %v", changed, err)
	}

	password := "replacement"
	if _, err := downloaders.Update(actor, created.ID, UpdateDownloaderInput{Password: &password, ClearPassword: true}, RequestContext{}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("clear+password error=%v", err)
	}
	if _, err := downloaders.Create(actor, DownloaderInput{Name: "Oversized", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Password: strings.Repeat("x", 4097), Enabled: true}, RequestContext{}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("oversized password error=%v", err)
	}
}

func TestDownloadWorkerPersistsTelemetryAndCancelRetainsProviderData(t *testing.T) {
	downloads, downloaders, queue, actor, client := downloadFixture(t)
	root := t.TempDir()
	storage := models.Storage{Name: "Staging", NameNormalized: "staging", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	configureDownloadStaging(t, queue, storage.ID)
	provider, err := downloaders.Create(actor, DownloaderInput{Name: "qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, DisplayName: "Movie", Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:abcdef"}}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := queue.Claim([]string{"download"})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	worker := NewDownloadWorker(downloads)
	worker.pollInterval = time.Millisecond
	result := finishScheduledDownload(t, worker, queue, claimed)
	if result.ErrorCode != "" || result.RetryAt != nil {
		t.Fatalf("result=%+v", result)
	}
	if err := queue.Complete(claimed.Job.ID, claimed.LeaseToken); err != nil {
		t.Fatal(err)
	}
	var task models.DownloadTask
	if err := queue.db.First(&task, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if task.Phase != models.DownloadTaskStatusCompleted || task.Progress == nil || *task.Progress != 100 || task.UploadSpeed == nil || *task.UploadSpeed != 2 {
		t.Fatalf("task=%+v", task)
	}

	// Pipeline cancellation is non-destructive: provider work and files remain.
	task.FinishedAt, task.Phase = nil, models.DownloadTaskStatusDownloading
	if err := queue.db.Save(&task).Error; err != nil {
		t.Fatal(err)
	}
	if err := worker.Interrupt(context.Background(), *claimed, "cancel"); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	cancelled, deleteData := client.cancelled, client.deleteData
	client.mu.Unlock()
	if !cancelled || deleteData {
		t.Fatalf("cancelled=%v deleteData=%v", cancelled, deleteData)
	}
	if err := queue.db.First(&task, "id = ?", created.ID).Error; err != nil || task.Phase != models.DownloadTaskStatusCancelled {
		t.Fatalf("cancelled task=%+v err=%v", task, err)
	}
}

func TestCancelPipelineAfterProviderCompletionRetainsFilesAndReleasesFollowClaim(t *testing.T) {
	downloads, downloaders, queue, actor, client := downloadFixture(t)
	actor.Permissions[authz.PermissionJobsControlOwn] = struct{}{}
	root := t.TempDir()
	storage := models.Storage{Name: "Pipeline cancel staging", NameNormalized: "pipeline cancel staging", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	configureDownloadStaging(t, queue, storage.ID)
	provider, err := downloaders.Create(actor, DownloaderInput{Name: "Pipeline cancel qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, DisplayName: "Completed provider, failed import", Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:pipeline-cancel"}}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", created.ID).Updates(map[string]any{"provider_task_id": "provider-output-retained", "phase": models.DownloadTaskStatusCompleted, "finished_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.Job{}).Where("id = ?", created.JobID).Updates(map[string]any{"status": models.JobStatusCompleted, "finished_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	transferJob, err := queue.Enqueue(EnqueueJobInput{OwnerID: actor.User.ID, JobType: "transfer", DisplayName: "Failed import", Payload: map[string]any{"transfer_task_id": "pipeline-cancel-transfer"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.Job{}).Where("id = ?", transferJob.ID).Updates(map[string]any{"status": models.JobStatusFailed, "finished_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	transfer := models.TransferTask{ID: "pipeline-cancel-transfer", OwnerID: actor.User.ID, JobID: transferJob.ID, DownloadTaskID: created.ID, LibraryID: 1, LibraryName: "Failed target", ManifestJSON: `{}`, SourceManifestJSON: `{}`, Phase: models.TransferTaskStatusFailed, CreatedAt: now, UpdatedAt: now, FinishedAt: &now}
	if err := queue.db.Create(&transfer).Error; err != nil {
		t.Fatal(err)
	}
	subscription := models.FollowSubscription{ID: "pipeline-cancel-follow", OwnerID: actor.User.ID, MediaType: "tv", TMDBID: 1, Title: "Pipeline cancel show", Status: models.FollowStatusActive, Revision: 1, LifecycleRevision: 1, ExecutionSnapshotJSON: `{}`, CreatedAt: now, UpdatedAt: now}
	if err := queue.db.Create(&subscription).Error; err != nil {
		t.Fatal(err)
	}
	claim := models.FollowEpisodeClaim{SubscriptionID: "pipeline-cancel-follow", SeasonNumber: 1, EpisodeNumber: 2, State: "downloading", DownloadTaskID: &created.ID, ResourceFingerprint: "pipeline-cancel-resource", UpdatedAt: now}
	if err := queue.db.Create(&claim).Error; err != nil {
		t.Fatal(err)
	}

	if err := downloads.CancelPipeline(context.Background(), actor, created.ID, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	client.cancelErr = downloadpkg.Error("downloader_task_not_found", false, errors.New("already removed"))
	if err := downloads.CancelPipeline(context.Background(), actor, created.ID, RequestContext{}); err != nil {
		t.Fatalf("repeat cancel after provider task-not-found must be idempotent: %v", err)
	}
	var task models.DownloadTask
	var downloadJob, persistedTransferJob models.Job
	if err := queue.db.First(&task, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	_ = queue.db.First(&downloadJob, "id = ?", created.JobID).Error
	_ = queue.db.First(&persistedTransferJob, "id = ?", transferJob.ID).Error
	if task.Phase != models.DownloadTaskStatusCancelled || downloadJob.Status != models.JobStatusCancelled || persistedTransferJob.Status != models.JobStatusCancelled {
		t.Fatalf("task=%+v downloadJob=%+v transferJob=%+v", task, downloadJob, persistedTransferJob)
	}
	if err := queue.db.First(&claim, "subscription_id = ? AND season_number = ? AND episode_number = ?", claim.SubscriptionID, 1, 2).Error; err != nil || claim.State != "failed" || claim.DownloadTaskID != nil {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	client.mu.Lock()
	providerCancelled, deleteData := client.cancelled, client.deleteData
	client.mu.Unlock()
	if !providerCancelled || deleteData {
		t.Fatalf("pipeline cancel touched provider: cancelled=%v deleteData=%v", providerCancelled, deleteData)
	}
	items, _, err := downloads.ListScoped(actor, DownloadListScopeHistory, 20)
	if err != nil || len(items) != 1 || items[0].ID != created.ID {
		t.Fatalf("history=%+v err=%v", items, err)
	}
}

func TestCancelPipelineCleansUpProviderTaskThatArrivesAfterSubmitRace(t *testing.T) {
	for _, test := range []struct {
		name                 string
		cancelErr, submitErr error
	}{
		{name: "late accepted identity"},
		{name: "late cleanup retries", cancelErr: downloadpkg.Error("downloader_unavailable", true, nil)},
		{name: "unknown submission survives restart and removal", submitErr: downloadpkg.Error("downloader_submit_unknown", true, cloudpkg.Error(cloudpkg.CodeMutationUnknown, true, nil))},
	} {
		t.Run(test.name, func(t *testing.T) {
			downloads, downloaders, queue, actor, client := downloadFixture(t)
			clock := &fakeQueueClock{now: time.Now().UTC()}
			queue.SetClock(clock)
			actor.Permissions[authz.PermissionJobsControlOwn] = struct{}{}
			root := t.TempDir()
			storage := models.Storage{Name: "Submit race", NameNormalized: "submit race", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`, CreatedAt: clock.now, UpdatedAt: clock.now}
			if err := queue.db.Create(&storage).Error; err != nil {
				t.Fatal(err)
			}
			configureDownloadStaging(t, queue, storage.ID)
			provider, err := downloaders.Create(actor, DownloaderInput{Name: "Submit race qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Enabled: true}, RequestContext{})
			if err != nil {
				t.Fatal(err)
			}
			subscription := models.FollowSubscription{ID: "pending-submit-follow", OwnerID: actor.User.ID, MediaType: "tv", TMDBID: 1, Title: "Pending submit", Status: models.FollowStatusActive, Revision: 1, LifecycleRevision: 1, ExecutionSnapshotJSON: `{}`, CreatedAt: clock.now, UpdatedAt: clock.now}
			if err := queue.db.Create(&subscription).Error; err != nil {
				t.Fatal(err)
			}
			created, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, DisplayName: "Submit race", Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:submit-race"}}, RequestContext{})
			if err != nil {
				t.Fatal(err)
			}
			claim := models.FollowEpisodeClaim{SubscriptionID: subscription.ID, SeasonNumber: 1, EpisodeNumber: 2, State: "downloading", DownloadTaskID: &created.ID, ResourceFingerprint: "pending-resource", UpdatedAt: clock.now}
			if err := queue.db.Create(&claim).Error; err != nil {
				t.Fatal(err)
			}
			claimed, err := queue.Claim([]string{"download"})
			if err != nil || claimed == nil {
				t.Fatalf("claim=%+v err=%v", claimed, err)
			}
			submitSeen, submitWait := make(chan struct{}, 1), make(chan struct{})
			client.submitSeen, client.submitWait, client.cancelErr, client.submitErr = submitSeen, submitWait, test.cancelErr, test.submitErr
			var release sync.Once
			unblock := func() { release.Do(func() { close(submitWait) }) }
			defer unblock()
			worker := NewDownloadWorker(downloads)
			resultCh := make(chan WorkerResult, 1)
			go func() {
				resultCh <- worker.Run(context.Background(), workerRuntime{queue: queue, job: *claimed}, *claimed)
			}()
			select {
			case <-submitSeen:
			case <-time.After(2 * time.Second):
				t.Fatal("submit did not start")
			}
			assertHeld := func() {
				t.Helper()
				followWorker := NewFollowSearchWorker(NewFollowService(queue.db, queue.audit, queue, nil, nil), nil)
				remaining, err := followWorker.excludeClaimedDownloads(context.Background(), subscription.ID, map[[2]int]struct{}{{1, 2}: {}})
				if err != nil || len(remaining) != 0 {
					t.Fatalf("Follow made an unresolved original episode eligible again: %+v %v", remaining, err)
				}
				var held models.FollowEpisodeClaim
				if err := queue.db.First(&held, "subscription_id=? AND season_number=1 AND episode_number=2", subscription.ID).Error; err != nil || held.DownloadTaskID == nil || *held.DownloadTaskID != created.ID || held.State != "downloading" {
					t.Fatalf("original reservation lost: %+v %v", held, err)
				}
				if !downloads.ControlPending(created.ID) {
					t.Fatal("accepted cancel did not retain pending cleanup")
				}
			}
			if err := downloads.CancelPipeline(context.Background(), actor, created.ID, RequestContext{}); err != nil {
				t.Fatal(err)
			}
			if _, err := queue.Control(actor, created.JobID, "cancel", RequestContext{}); err != nil {
				t.Fatal("repeat queue cancellation", err)
			}
			assertHeld()
			history, _, err := downloads.ListScoped(actor, DownloadListScopeHistory, 20)
			if err != nil || len(history) != 1 || !history[0].ControlPending || history[0].Phase != models.DownloadTaskStatusCancelled {
				t.Fatalf("cancelled pending history=%+v err=%v", history, err)
			}
			restricted := actor
			restricted.ResourceAccessPolicies = map[string]ResourceAccessPolicy{models.ResourceAccessScopeDownloaderUse: {Mode: models.ResourceAccessModeAllowlist}}
			history, _, err = downloads.ListScoped(restricted, DownloadListScopeHistory, 20)
			if err != nil || len(history) != 1 || !history[0].ControlPending || history[0].DownloaderID != nil {
				t.Fatalf("restricted pending lifecycle was lost or exposed provider details: %+v %v", history, err)
			}
			if test.submitErr != nil {
				if err := downloads.Delete(context.Background(), actor, created.ID, true, RequestContext{}); ErrorCode(err) != "downloader_submission_unconfirmed" {
					t.Fatal("unknown deletion may delete files", err)
				}
				if err := downloads.Delete(context.Background(), actor, created.ID, false, RequestContext{}); err != nil {
					t.Fatal(err)
				}
				if err := downloads.Delete(context.Background(), actor, created.ID, false, RequestContext{}); err != nil {
					t.Fatal("repeat record removal", err)
				}
				rows, total, err := downloads.ListScoped(actor, DownloadListScopeAll, 20)
				if err != nil || total != 0 || len(rows) != 0 {
					t.Fatalf("hidden record visible: %+v %v", rows, err)
				}
				if _, err := queue.Get(actor, created.JobID); err == nil {
					t.Fatal("private reconcile job exposed")
				}
				assertHeld()
			}
			unblock()
			result := <-resultCh
			if result.RetryAt != nil {
				err = queue.RetryLater(claimed.Job.ID, claimed.LeaseToken, result.ErrorCode, result.ErrorMessage, *result.RetryAt)
			} else if result.ErrorCode != "" {
				err = queue.Fail(claimed.Job.ID, claimed.LeaseToken, result.ErrorCode, result.ErrorMessage)
			} else {
				err = queue.Complete(claimed.Job.ID, claimed.LeaseToken)
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.cancelErr != nil || test.submitErr != nil {
				assertHeld()
				client.mu.Lock()
				client.cancelErr = nil
				client.reconcileErr = downloadpkg.Error("downloader_submission_unconfirmed", true, nil)
				client.mu.Unlock()
				// Restarted workers only query the original source. More than the
				// ordinary failure budget must neither Submit nor release its claim.
				rounds := 1
				if test.submitErr != nil {
					rounds = 5
				}
				for n := 0; n < rounds; n++ {
					clock.now = clock.now.Add(2 * time.Minute)
					if err := queue.PromoteDueRetries(); err != nil {
						t.Fatal(err)
					}
					next, err := queue.Claim([]string{"download"})
					if err != nil || next == nil {
						t.Fatalf("private recovery not scheduled: %+v %v", next, err)
					}
					if n == rounds-1 {
						client.mu.Lock()
						client.reconcileErr = nil
						client.mu.Unlock()
					}
					restarted := NewDownloadWorker(downloads)
					recovered := restarted.Run(context.Background(), workerRuntime{queue: queue, job: *next}, *next)
					if recovered.RetryAt != nil {
						err = queue.RetryLater(next.Job.ID, next.LeaseToken, recovered.ErrorCode, recovered.ErrorMessage, *recovered.RetryAt)
					} else if recovered.ErrorCode == "" {
						err = queue.Complete(next.Job.ID, next.LeaseToken)
					} else {
						t.Fatalf("unexpected recovery %+v", recovered)
					}
					if err != nil {
						t.Fatal(err)
					}
					if n < rounds-1 {
						assertHeld()
					}
				}
			}
			client.mu.Lock()
			cancelled, deleteData, submits := client.cancelled, client.deleteData, client.submits
			client.mu.Unlock()
			if !cancelled || deleteData || submits != 1 {
				t.Fatalf("cleanup cancelled=%v deleteData=%v submissions=%d", cancelled, deleteData, submits)
			}
			var task models.DownloadTask
			if err := queue.db.First(&task, "id=?", created.ID).Error; err != nil || task.Phase != models.DownloadTaskStatusCancelled || task.ProviderTaskID != "provider-hash" || task.LastErrorCode != "" {
				t.Fatalf("cancelled task=%+v err=%v", task, err)
			}
			if err := queue.db.First(&claim, "subscription_id=? AND season_number=1 AND episode_number=2", subscription.ID).Error; err != nil || claim.State != "failed" || claim.DownloadTaskID != nil {
				t.Fatalf("settled claim=%+v err=%v", claim, err)
			}
			var job models.Job
			if err := queue.db.First(&job, "id=?", created.JobID).Error; err != nil || job.Status != models.JobStatusCancelled || job.FailureRetryCount != 0 {
				t.Fatalf("cleanup job=%+v err=%v", job, err)
			}
		})
	}
}

func TestCancelPipelineRetriesProviderCleanupWhenIdentityChangesAfterCleanup(t *testing.T) {
	downloads, downloaders, queue, actor, client := downloadFixture(t)
	actor.Permissions[authz.PermissionJobsControlOwn] = struct{}{}
	root := t.TempDir()
	storage := models.Storage{Name: "Provider identity race", NameNormalized: "provider identity race", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	configureDownloadStaging(t, queue, storage.ID)
	provider, err := downloaders.Create(actor, DownloaderInput{Name: "Provider identity race qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, DisplayName: "Provider identity race", Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:provider-identity-race"}}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", created.ID).Updates(map[string]any{"provider_task_id": "provider-old", "phase": models.DownloadTaskStatusDownloading}).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.Job{}).Where("id = ?", created.JobID).Update("status", models.JobStatusRunning).Error; err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	client.cancelHook = func(id string) {
		once.Do(func() {
			if id != "provider-old" {
				t.Errorf("first cleanup id=%q", id)
				return
			}
			if updateErr := queue.db.Model(&models.DownloadTask{}).Where("id = ?", created.ID).Update("provider_task_id", "provider-new").Error; updateErr != nil {
				t.Errorf("persist raced provider identity: %v", updateErr)
			}
		})
	}

	if err := downloads.CancelPipeline(context.Background(), actor, created.ID, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	cancelIDs := append([]string(nil), client.cancelIDs...)
	deleteData := client.deleteData
	client.mu.Unlock()
	if len(cancelIDs) != 2 || cancelIDs[0] != "provider-old" || cancelIDs[1] != "provider-new" || deleteData {
		t.Fatalf("provider cleanup calls=%v deleteData=%v", cancelIDs, deleteData)
	}
	var task models.DownloadTask
	if err := queue.db.First(&task, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if task.ProviderTaskID != "provider-new" || task.Phase != models.DownloadTaskStatusCancelled {
		t.Fatalf("final task=%+v", task)
	}
}

func TestExplicitRetryDurablyClearsDownloadErrorAndActiveTelemetryCannotResurrectIt(t *testing.T) {
	downloads, downloaders, queue, actor, _ := downloadFixture(t)
	actor.Permissions[authz.PermissionJobsControlOwn] = struct{}{}
	root := t.TempDir()
	storage := models.Storage{Name: "Retry staging", NameNormalized: "retry staging", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	configureDownloadStaging(t, queue, storage.ID)
	provider, err := downloaders.Create(actor, DownloaderInput{Name: "Retry qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, DisplayName: "Retry stale error", Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:retry-stale-error"}}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	failedAt := time.Now().UTC()
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", created.ID).Updates(map[string]any{
		"provider_task_id": "provider-hash", "provider_status": "checkingResumeData", "phase": models.DownloadTaskStatusFailed,
		"last_error_code": "downloader_category_outside_staging", "last_error_message": "下载任务执行失败", "finished_at": failedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.Job{}).Where("id = ?", created.JobID).Updates(map[string]any{
		"status": models.JobStatusFailed, "last_error_code": "downloader_category_outside_staging", "last_error_message": "下载任务执行失败", "finished_at": failedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}

	if _, err := queue.Control(actor, created.JobID, "retry", RequestContext{}); err != nil {
		t.Fatal(err)
	}
	var job models.Job
	if err := queue.db.First(&job, "id = ?", created.JobID).Error; err != nil {
		t.Fatal(err)
	}
	var task models.DownloadTask
	if err := queue.db.First(&task, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if job.Status != models.JobStatusQueued || job.LastErrorCode != "" || job.LastErrorMessage != "" || job.FinishedAt != nil {
		t.Fatalf("retry job state=%+v", job)
	}
	if task.LastErrorCode != "" || task.LastErrorMessage != "" || task.FinishedAt != nil {
		t.Fatalf("retry download state=%+v", task)
	}
	listed, _, err := downloads.ListScoped(actor, DownloadListScopeActive, 10)
	if err != nil || len(listed) != 1 || listed[0].LastErrorMessage != "" {
		t.Fatalf("retry list=%+v err=%v", listed, err)
	}

	// Meaningful preclassification warnings remain visible while the provider
	// is active; they are not stale terminal failures.
	task.Phase, task.ScrapeStatus, task.LastErrorCode, task.LastErrorMessage, task.FinishedAt = models.DownloadTaskStatusDownloading, "fallback_unrecognized", "tmdb_no_match", "未识别，将在下载完成后复核", nil
	if err := queue.db.Save(&task).Error; err != nil {
		t.Fatal(err)
	}
	progress, completed, total, speed := 0.0, int64(0), int64(1024), int64(0)
	if err := NewDownloadWorker(downloads).persistTelemetry(&task, downloadpkg.Task{ID: task.ProviderTaskID, Status: "stalledDL", Progress: &progress, BytesCompleted: &completed, BytesTotal: &total, DownloadSpeed: &speed}); err != nil {
		t.Fatal(err)
	}
	if task.LastErrorCode != "tmdb_no_match" || task.LastErrorMessage != "未识别，将在下载完成后复核" {
		t.Fatalf("active classification warning was cleared: %+v", task)
	}

	// An authoritative provider failure is the current error, never a stale
	// terminal one, and therefore must not be cleared by telemetry persistence.
	task.Phase, task.LastErrorCode, task.LastErrorMessage, task.FinishedAt = models.DownloadTaskStatusFailed, "downloader_provider_failed", "下载器报告任务失败", &failedAt
	if err := queue.db.Save(&task).Error; err != nil {
		t.Fatal(err)
	}
	if err := NewDownloadWorker(downloads).persistTelemetry(&task, downloadpkg.Task{ID: task.ProviderTaskID, Status: "error", Failed: true}); err != nil {
		t.Fatal(err)
	}
	if task.Phase != models.DownloadTaskStatusFailed || task.LastErrorCode != "downloader_provider_failed" || task.LastErrorMessage != "下载器报告任务失败" || task.FinishedAt == nil {
		t.Fatalf("current provider failure was cleared: %+v", task)
	}

	// Also converge legacy retries accepted before this fix: an authoritative
	// active provider sample clears only a terminal DownloadTask error.
	task.Phase, task.LastErrorCode, task.LastErrorMessage, task.FinishedAt = models.DownloadTaskStatusFailed, "old_terminal", "old terminal failure", &failedAt
	if err := queue.db.Save(&task).Error; err != nil {
		t.Fatal(err)
	}
	if err := NewDownloadWorker(downloads).persistTelemetry(&task, downloadpkg.Task{ID: task.ProviderTaskID, Status: "stalledDL", Progress: &progress, BytesCompleted: &completed, BytesTotal: &total, DownloadSpeed: &speed}); err != nil {
		t.Fatal(err)
	}
	if task.Phase != models.DownloadTaskStatusDownloading || task.ProviderStatus != "stalledDL" || task.LastErrorCode != "" || task.LastErrorMessage != "" || task.FinishedAt != nil {
		t.Fatalf("active telemetry state=%+v", task)
	}

	now := time.Now().UTC()
	subscription := models.FollowSubscription{ID: "follow-download-terminal-sync", OwnerID: actor.User.ID, MediaType: "tv", TMDBID: 100, Title: "Terminal sync", Status: models.FollowStatusActive, Revision: 1, LifecycleRevision: 1, ExecutionSnapshotJSON: `{}`, CreatedAt: now, UpdatedAt: now}
	if err := queue.db.Create(&subscription).Error; err != nil {
		t.Fatal(err)
	}
	claim := models.FollowEpisodeClaim{SubscriptionID: subscription.ID, SeasonNumber: 1, EpisodeNumber: 1, State: "downloading", DownloadTaskID: &task.ID, ResourceFingerprint: "terminal-sync", UpdatedAt: now}
	if err := queue.db.Create(&claim).Error; err != nil {
		t.Fatal(err)
	}
	if err := NewDownloadWorker(downloads).markFailure(task, "downloader_provider_failed", "本次重试仍失败", true); err != nil {
		t.Fatal(err)
	}
	if err := queue.db.First(&claim, "subscription_id = ? AND season_number = ? AND episode_number = ?", subscription.ID, 1, 1).Error; err != nil || claim.State != "failed" || claim.DownloadTaskID != nil {
		t.Fatalf("terminal follow claim=%+v err=%v", claim, err)
	}
	if err := queue.db.Model(&models.Job{}).Where("id = ?", created.JobID).Updates(map[string]any{"status": models.JobStatusFailed, "last_error_code": "downloader_provider_failed", "last_error_message": "本次重试仍失败"}).Error; err != nil {
		t.Fatal(err)
	}
	listed, _, err = downloads.ListScoped(actor, DownloadListScopeActive, 10)
	if err != nil || len(listed) != 1 || listed[0].LastErrorCode != "downloader_provider_failed" || listed[0].LastErrorMessage != "本次重试仍失败" {
		t.Fatalf("new failure list=%+v err=%v", listed, err)
	}
}

func TestExplicitDownloadRetryRollsBackJobWhenDomainStateIsMissing(t *testing.T) {
	downloads, downloaders, queue, actor, _ := downloadFixture(t)
	actor.Permissions[authz.PermissionJobsControlOwn] = struct{}{}
	root := t.TempDir()
	storage := models.Storage{Name: "Retry rollback", NameNormalized: "retry rollback", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	configureDownloadStaging(t, queue, storage.ID)
	provider, err := downloaders.Create(actor, DownloaderInput{Name: "Rollback qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, DisplayName: "Retry rollback", Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:retry-rollback"}}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	failedAt := time.Now().UTC()
	if err := queue.db.Model(&models.Job{}).Where("id = ?", created.JobID).Updates(map[string]any{
		"status": models.JobStatusFailed, "last_error_code": "downloader_provider_failed", "last_error_message": "下载任务执行失败", "finished_at": failedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Delete(&models.DownloadTask{}, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}

	if _, err := queue.Control(actor, created.JobID, "retry", RequestContext{}); ErrorCode(err) != "download_state_persist_failed" {
		t.Fatalf("retry error=%v code=%s", err, ErrorCode(err))
	}
	var job models.Job
	if err := queue.db.First(&job, "id = ?", created.JobID).Error; err != nil {
		t.Fatal(err)
	}
	if job.Status != models.JobStatusFailed || job.LastErrorCode != "downloader_provider_failed" || job.LastErrorMessage != "下载任务执行失败" || job.FinishedAt == nil {
		t.Fatalf("retry transaction did not roll back: %+v", job)
	}
}

func TestTorrentSourceValidation(t *testing.T) {
	valid := []byte("d4:infod4:name4:testee")
	source, name, err := normalizeDownloadSource(DownloadSourceInput{Kind: downloadpkg.SourceTorrent, Filename: "movie.torrent", Torrent: valid}, "")
	if err != nil || source.Filename != "movie.torrent" || name != "movie" {
		t.Fatalf("source=%+v name=%q err=%v", source, name, err)
	}
	if _, _, err := normalizeDownloadSource(DownloadSourceInput{Kind: downloadpkg.SourceTorrent, Filename: "movie.txt", Torrent: valid}, ""); ErrorCode(err) != CodeDownloadTorrentInvalid {
		t.Fatalf("error=%v", err)
	}
	if _, _, err := normalizeDownloadSource(DownloadSourceInput{Kind: downloadpkg.SourceTorrent, Filename: "movie.torrent", Torrent: make([]byte, downloadpkg.MaxTorrentBytes+1)}, ""); ErrorCode(err) != CodeDownloadTorrentInvalid {
		t.Fatalf("error=%v", err)
	}
}

func TestDownloadWorkerReconcilesExistingProviderTaskWithoutResubmit(t *testing.T) {
	downloads, downloaders, queue, actor, client := downloadFixture(t)
	var businessLog bytes.Buffer
	downloads.log = zerolog.New(zerolog.SyncWriter(&businessLog)).With().Timestamp().Logger()
	root := t.TempDir()
	storage := models.Storage{Name: "Staging", NameNormalized: "staging", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	configureDownloadStaging(t, queue, storage.ID)
	provider, err := downloaders.Create(actor, DownloaderInput{Name: "qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, DisplayName: "Resume", Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:resume"}}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := queue.Claim([]string{"download"})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	worker := NewDownloadWorker(downloads)
	client.submitErr = downloadpkg.WithFailureStage(downloadpkg.Error("downloader_unavailable", true, cloudpkg.Error(cloudpkg.CodeUnavailable, true, errors.Join(context.DeadlineExceeded, downloadProviderDiagnosticError{}))), downloadpkg.FailureOfflineSubmission)
	first := worker.Run(context.Background(), workerRuntime{queue: queue, job: *claimed}, *claimed)
	if first.ErrorCode != "downloader_unavailable" || first.RetryAt == nil || !strings.Contains(first.ErrorMessage, "原离线任务") || !strings.Contains(first.ErrorMessage, "超时") {
		t.Fatalf("submit failure=%+v", first)
	}
	var failed models.DownloadTask
	if err := queue.db.First(&failed, "id = ?", created.ID).Error; err != nil || failed.ProviderTaskID != "" || failed.LastErrorMessage != first.ErrorMessage || failed.LastSampledAt != nil {
		t.Fatalf("submit failure reached telemetry or lost diagnostic: %+v err=%v", failed, err)
	}
	if !strings.Contains(businessLog.String(), `"event_code":"download.provider_submit_failed"`) || !strings.Contains(businessLog.String(), `"phase":"offline_submission"`) || !strings.Contains(businessLog.String(), `"request_timeout":true`) || strings.Contains(businessLog.String()+first.ErrorMessage, "private-output-id") {
		t.Fatalf("missing or unsafe submit diagnostic: %s", businessLog.String())
	}
	if !strings.Contains(businessLog.String(), `"provider_operation":"reconcile"`) || !strings.Contains(businessLog.String(), `"provider_http_status":200`) || !strings.Contains(businessLog.String(), `"provider_errno":12345`) || !strings.Contains(first.ErrorMessage, "HTTP 200") || !strings.Contains(first.ErrorMessage, "提供方编号 12345") {
		t.Fatal("known finite provider facts did not survive worker failure persistence")
	}
	// A later confirmed original identity resumes telemetry without Submit.
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", created.ID).Updates(map[string]any{"provider_task_id": "provider-hash", "phase": models.DownloadTaskStatusDownloading}).Error; err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	client.gets, client.submitErr = 1, nil // next original-task sample is completed
	client.mu.Unlock()
	worker.pollInterval = time.Millisecond
	result := worker.Run(context.Background(), workerRuntime{queue: queue, job: *claimed}, *claimed)
	if result.ErrorCode != "" || result.RetryAt != nil {
		t.Fatalf("result=%+v", result)
	}
	client.mu.Lock()
	submits := client.submits
	client.mu.Unlock()
	if submits != 1 {
		t.Fatalf("restart reconciliation resubmitted provider task %d time(s)", submits)
	}
}

func TestDownloadWorkerScheduledReconnectKeepsOriginalAcquisition(t *testing.T) {
	downloads, downloaders, queue, actor, client := downloadFixture(t)
	var businessLog bytes.Buffer
	downloads.log = zerolog.New(zerolog.SyncWriter(&businessLog)).With().Timestamp().Logger()
	root := t.TempDir()
	storage := models.Storage{Name: "Reconnect staging", NameNormalized: "reconnect staging", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	configureDownloadStaging(t, queue, storage.ID)
	provider, err := downloaders.Create(actor, DownloaderInput{Name: "Reconnect qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, DisplayName: "Reconnect", Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:reconnect"}}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", created.ID).Updates(map[string]any{"provider_task_id": "provider-hash", "phase": models.DownloadTaskStatusDownloading}).Error; err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	client.getErrors = []error{
		downloadpkg.Error("downloader_unavailable", true, errors.New("offline")),
		downloadpkg.Error("downloader_unavailable", true, errors.New("offline")),
	}
	client.mu.Unlock()
	claimed, err := queue.Claim([]string{"download"})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	worker := NewDownloadWorker(downloads)
	worker.pollInterval = time.Millisecond
	worker.reconnectInterval = time.Millisecond
	result := finishScheduledDownload(t, worker, queue, claimed)
	if result.ErrorCode != "" || result.RetryAt != nil {
		t.Fatalf("result=%+v", result)
	}
	if err := queue.Complete(created.JobID, claimed.LeaseToken); err != nil {
		t.Fatal(err)
	}
	var job models.Job
	if err := queue.db.First(&job, "id = ?", created.JobID).Error; err != nil {
		t.Fatal(err)
	}
	var attempts int64
	if err := queue.db.Model(&models.JobAttempt{}).Where("job_id = ?", created.JobID).Count(&attempts).Error; err != nil {
		t.Fatal(err)
	}
	var task models.DownloadTask
	if err := queue.db.First(&task, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	submits := client.submits
	client.mu.Unlock()
	if job.AttemptCount != 1 || attempts != 1 || job.FailureRetryCount != 0 || submits != 0 || task.LastErrorCode != "" || task.LastErrorMessage != "" {
		t.Fatalf("job=%+v attempts=%d submits=%d task=%+v", job, attempts, submits, task)
	}
	if len(businessRecords(t, &businessLog, "event_code", "download.reconnect_wait")) != 1 || len(businessRecords(t, &businessLog, "event_code", "download.reconnected")) != 1 {
		t.Fatal("reconnect narrative was missing or repeated every poll")
	}
	terminals := businessRecords(t, &businessLog, "event_code", "download.finish")
	if len(terminals) != 1 || terminals[0]["outcome"] != "finished" || terminals[0]["job_id"] != created.JobID {
		t.Fatalf("download terminal fact=%+v", terminals)
	}
	for _, forbidden := range []string{"provider-hash", "magnet:", root, "http://qbit.example.test", "媒体已入库"} {
		if strings.Contains(businessLog.String(), forbidden) {
			t.Fatalf("download log exposed private/fictional facts: %q", forbidden)
		}
	}
}

func TestDownloadWorkerReconnectWaitStopsWithContext(t *testing.T) {
	downloads, downloaders, queue, actor, client := downloadFixture(t)
	root := t.TempDir()
	storage := models.Storage{Name: "Reconnect cancel staging", NameNormalized: "reconnect cancel staging", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	configureDownloadStaging(t, queue, storage.ID)
	provider, err := downloaders.Create(actor, DownloaderInput{Name: "Reconnect cancel qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, DisplayName: "Reconnect cancel", Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:reconnect-cancel"}}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", created.ID).Updates(map[string]any{"provider_task_id": "provider-hash", "phase": models.DownloadTaskStatusDownloading}).Error; err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	client.getErr = downloadpkg.Error("downloader_unavailable", true, errors.New("offline"))
	client.mu.Unlock()
	claimed, err := queue.Claim([]string{"download"})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	worker := NewDownloadWorker(downloads)
	worker.reconnectInterval = time.Hour
	result := worker.Run(context.Background(), workerRuntime{queue: queue, job: *claimed}, *claimed)
	if result.ErrorCode != "" || result.RetryAt == nil {
		t.Fatalf("bounded reconnect=%+v", result)
	}
	if err := queue.RetryLater(created.JobID, claimed.LeaseToken, "", "", *result.RetryAt); err != nil {
		t.Fatal(err)
	}
	var saved models.Job
	if err := queue.db.First(&saved, "id = ?", created.JobID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.Status != models.JobStatusRetryWait || saved.FailureRetryCount != 0 || saved.LeaseTokenHash != "" {
		t.Fatalf("reconnect retained execution capacity: %+v", saved)
	}
	var admission models.DownloadAdmission
	if err := queue.db.First(&admission, "acquisition_key = ?", "task:"+created.ID).Error; err != nil || admission.State != "held" {
		t.Fatalf("original acquisition lost: %+v %v", admission, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result := worker.Run(ctx, workerRuntime{queue: queue, job: *claimed}, *claimed); result.RetryAt != nil || result.ErrorCode != "" {
		t.Fatalf("cancelled observation=%+v", result)
	}
	// An old terminal tracking Job is independently observed, not restarted.
	if err := queue.db.Model(&models.Job{}).Where("id = ?", created.JobID).Updates(map[string]any{"status": models.JobStatusFailed, "last_error_code": "old_tracking_failed"}).Error; err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	client.getErr = nil
	client.seedTask = &downloadpkg.Task{ID: "provider-hash", Status: "completed", Completed: true}
	client.mu.Unlock()
	if err := worker.RecoverOfflineStops(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := queue.db.First(&admission, "acquisition_key = ?", "task:"+created.ID).Error; err != nil || admission.State != "released" {
		t.Fatalf("legacy completed native task retained capacity: %+v %v", admission, err)
	}
	if err := queue.db.First(&saved, "id = ?", created.JobID).Error; err != nil || saved.Status != models.JobStatusFailed {
		t.Fatal("admission observation revived the old Job")
	}
	client.mu.Lock()
	writes := client.submits
	client.mu.Unlock()
	if writes != 0 {
		t.Fatal("legacy admission reconciliation submitted native work")
	}

}

func TestManagedQBittorrentTagCleanupIsDurableAndRetryable(t *testing.T) {
	downloads, downloaders, queue, actor, client := downloadFixture(t)
	root := t.TempDir()
	storage := models.Storage{Name: "Tag cleanup staging", NameNormalized: "tag cleanup staging", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	configureDownloadStaging(t, queue, storage.ID)
	provider, err := downloaders.Create(actor, DownloaderInput{Name: "Tag cleanup qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, DisplayName: "Tag cleanup", Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:tag-cleanup"}}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", created.ID).Update("provider_task_id", "provider-hash").Error; err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	client.cleanupErr = downloadpkg.Error("downloader_unavailable", true, errors.New("offline"))
	client.mu.Unlock()
	cleaned, err := downloads.ReconcileManagedProviderTags(context.Background(), 10)
	if err != nil || cleaned != 0 {
		t.Fatalf("first cleanup=%d err=%v", cleaned, err)
	}
	var task models.DownloadTask
	if err := queue.db.First(&task, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if task.ProviderTag == "" {
		t.Fatal("failed upstream cleanup cleared the durable tag marker")
	}
	wantTag := task.ProviderTag
	client.mu.Lock()
	client.cleanupErr = nil
	client.cleanupHook = func(tag string) {
		if err := queue.db.Model(&models.DownloadTask{}).Where("id = ? AND provider_tag = ?", created.ID, tag).Update("provider_tag", "").Error; err != nil {
			t.Errorf("concurrent marker cleanup: %v", err)
		}
	}
	client.mu.Unlock()
	cleaned, err = downloads.ReconcileManagedProviderTags(context.Background(), 10)
	if err != nil || cleaned != 1 {
		t.Fatalf("second cleanup=%d err=%v", cleaned, err)
	}
	if err := queue.db.First(&task, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	deletedTags := append([]string(nil), client.deletedTags...)
	client.mu.Unlock()
	if task.ProviderTag != "" || len(deletedTags) != 1 || deletedTags[0] != wantTag {
		t.Fatalf("task=%+v deleted_tags=%v want=%q", task, deletedTags, wantTag)
	}
}

func TestDownloadWorkerRecoversLegacyLeaseFailureWithoutResubmit(t *testing.T) {
	downloads, downloaders, queue, actor, client := downloadFixture(t)
	root := t.TempDir()
	storage := models.Storage{Name: "Legacy recovery staging", NameNormalized: "legacy recovery staging", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	configureDownloadStaging(t, queue, storage.ID)
	provider, err := downloaders.Create(actor, DownloaderInput{Name: "Legacy recovery qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, DisplayName: "Legacy recovery", Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:legacy-recovery"}}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", created.ID).Updates(map[string]any{
		"provider_task_id":   "provider-hash",
		"provider_status":    "downloading",
		"phase":              models.DownloadTaskStatusDownloading,
		"last_error_code":    "downloader_unavailable",
		"last_error_message": "下载器暂时不可用，任务将自动重试",
	}).Error; err != nil {
		t.Fatal(err)
	}
	claimed, err := queue.Claim([]string{"download"})
	if err != nil || claimed == nil {
		t.Fatalf("initial claim=%+v err=%v", claimed, err)
	}
	failedAt := time.Now().UTC()
	if err := queue.db.Transaction(func(tx *gorm.DB) error {
		updates := map[string]any{
			"status":             models.JobStatusFailed,
			"last_error_code":    codeQueueWorkerLeaseExpired,
			"last_error_message": "Worker connection was lost; the job will resume from its checkpoint.",
			"finished_at":        failedAt,
			"updated_at":         failedAt,
		}
		releaseLease(updates)
		if err := tx.Model(&models.Job{}).Where("id = ?", created.JobID).Updates(updates).Error; err != nil {
			return err
		}
		return tx.Model(&models.JobAttempt{}).
			Where("job_id = ? AND attempt_number = ?", created.JobID, claimed.Job.AttemptCount).
			Updates(map[string]any{"status": models.JobStatusFailed, "safe_error_code": codeQueueWorkerLeaseExpired, "safe_error_message": "lease expired", "finished_at": failedAt}).Error
	}); err != nil {
		t.Fatal(err)
	}

	if err := queue.RecoverExpiredLeases(); err != nil {
		t.Fatal(err)
	}
	if err := queue.PromoteDueRetries(); err != nil {
		t.Fatal(err)
	}
	claimed, err = queue.Claim([]string{"download"})
	if err != nil || claimed == nil || claimed.Job.ID != created.JobID {
		t.Fatalf("recovery claim=%+v err=%v", claimed, err)
	}
	worker := NewDownloadWorker(downloads)
	worker.pollInterval = time.Millisecond
	result := finishScheduledDownload(t, worker, queue, claimed)
	if result.ErrorCode != "" || result.RetryAt != nil {
		t.Fatalf("result=%+v", result)
	}
	if err := queue.Complete(created.JobID, claimed.LeaseToken); err != nil {
		t.Fatal(err)
	}

	client.mu.Lock()
	submits := client.submits
	client.mu.Unlock()
	if submits != 0 {
		t.Fatalf("legacy recovery resubmitted provider task %d time(s)", submits)
	}
	var task models.DownloadTask
	if err := queue.db.First(&task, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	var job models.Job
	if err := queue.db.First(&job, "id = ?", created.JobID).Error; err != nil {
		t.Fatal(err)
	}
	if task.Phase != models.DownloadTaskStatusCompleted || task.LastErrorCode != "" || task.LastErrorMessage != "" || job.Status != models.JobStatusCompleted || job.LastErrorCode != "" || job.LastErrorMessage != "" {
		t.Fatalf("task=%+v job=%+v", task, job)
	}
}

func TestDownloadWorkerExplicitRetryReplacesFailedPan115TaskWithoutDeletingFiles(t *testing.T) {
	downloads, downloaders, queue, actor, client := downloadFixture(t)
	root := t.TempDir()
	storage := models.Storage{Name: "Staging retry", NameNormalized: "staging retry", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	configureDownloadStaging(t, queue, storage.ID)
	provider, err := downloaders.Create(actor, DownloaderInput{Name: "qBit retry fixture", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, DisplayName: "Retry", Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:retry"}}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", created.ID).Updates(map[string]any{
		"provider_task_id": "old-provider-hash", "provider_output_id": "partial-output", "provider_status": "failed",
		"phase": models.DownloadTaskStatusFailed, "last_error_code": "downloader_provider_failed", "last_error_message": "failed",
	}).Error; err != nil {
		t.Fatal(err)
	}
	var task models.DownloadTask
	if err := queue.db.First(&task, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	worker := NewDownloadWorker(downloads)
	failedProvider := downloadpkg.Task{ID: "old-provider-hash", Status: "failed", Failed: true}
	client.mu.Lock()
	client.seedTask = &failedProvider
	client.mu.Unlock()
	if err := worker.resetFailedPan115ForExplicitRetry(context.Background(), &task, client, models.DownloaderTypePan115Offline); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	cancelled, deleteData := client.cancelled, client.deleteData
	client.mu.Unlock()
	if !cancelled || deleteData {
		t.Fatalf("cancelled=%v deleteData=%v", cancelled, deleteData)
	}
	var persisted models.DownloadTask
	if err := queue.db.First(&persisted, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.ProviderTaskID != "" || persisted.ProviderOutputID != "" || persisted.Phase != models.DownloadTaskStatusQueued || persisted.LastErrorCode != "" {
		t.Fatalf("persisted=%+v", persisted)
	}

	client.mu.Lock()
	client.cancelled = false
	client.mu.Unlock()
	persisted.Phase = models.DownloadTaskStatusFailed
	persisted.ProviderTaskID = "completed-provider-hash"
	persisted.LastErrorCode = CodeTransferMediaUnrecognized
	completedProvider := downloadpkg.Task{ID: "completed-provider-hash", Status: "completed", Completed: true}
	client.mu.Lock()
	client.seedTask = &completedProvider
	client.mu.Unlock()
	if err := worker.resetFailedPan115ForExplicitRetry(context.Background(), &persisted, client, models.DownloaderTypePan115Offline); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	cancelled = client.cancelled
	client.mu.Unlock()
	if cancelled {
		t.Fatal("recognition/import failure must not delete or resubmit a completed 115 task")
	}

	client.mu.Lock()
	client.cancelled = false
	client.seedTask = &downloadpkg.Task{ID: "active-provider-hash", Status: "running"}
	client.mu.Unlock()
	persisted.ProviderTaskID = "active-provider-hash"
	if err := worker.resetFailedPan115ForExplicitRetry(context.Background(), &persisted, client, models.DownloaderTypePan115Offline); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	cancelled = client.cancelled
	client.mu.Unlock()
	if cancelled || persisted.ProviderTaskID != "active-provider-hash" {
		t.Fatalf("healthy 115 provider task was replaced: cancelled=%v task=%+v", cancelled, persisted)
	}

	client.mu.Lock()
	client.cancelled = false
	client.seedTask = nil
	client.getErr = downloadpkg.Error("downloader_task_not_found", false, nil)
	client.mu.Unlock()
	persisted.ProviderTaskID = "missing-provider-hash"
	if err := worker.resetFailedPan115ForExplicitRetry(context.Background(), &persisted, client, models.DownloaderTypePan115Offline); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	cancelled = client.cancelled
	client.mu.Unlock()
	if cancelled || persisted.ProviderTaskID != "" || persisted.Phase != models.DownloadTaskStatusQueued {
		t.Fatalf("missing 115 provider task did not reset safely: cancelled=%v task=%+v", cancelled, persisted)
	}
}

func TestDownloadWorkerAutomaticallyRoutesUnrecognizedWithoutTMDBCredential(t *testing.T) {
	queue, actor, _ := queueFixture(t)
	for _, code := range []string{authz.PermissionDownloadersCreate, authz.PermissionDownloadsCreate, authz.PermissionDownloadsReadAll, authz.PermissionJobsRespond} {
		actor.Permissions[code] = struct{}{}
	}
	persistFixtureAuthority(t, queue.db, actor)
	store, err := credential.Open(filepath.Join(t.TempDir(), "credentials.key"), "")
	if err != nil {
		t.Fatal(err)
	}
	client := &metadataDownloadClient{stubDownloadClient: &stubDownloadClient{}}
	registry := downloadpkg.NewRegistry()
	if err := registry.Register(models.DownloaderTypeQBittorrent, downloadpkg.Capabilities{}, func(downloadpkg.Config) (downloadpkg.Client, error) { return client, nil }); err != nil {
		t.Fatal(err)
	}
	downloaders := NewDownloaderService(queue.db, queue.audit, store, registry)
	settings := NewDownloadSettingsService(queue.db, queue.audit)
	downloads := NewDownloadService(queue.db, queue.audit, store, downloaders, settings, queue, zerolog.Nop())
	root := t.TempDir()
	storage := models.Storage{Name: "Staging", NameNormalized: "waiting staging", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	configureDownloadStaging(t, queue, storage.ID)
	provider, err := downloaders.Create(actor, DownloaderInput{Name: "qBit metadata", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:metadata"}}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := queue.Claim([]string{"download"})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	worker := NewDownloadWorker(downloads)
	worker.pollInterval = time.Millisecond
	result := finishScheduledDownload(t, worker, queue, claimed)
	if result.Wait != nil || result.ErrorCode != "" || !client.metadataOnly || !client.paused || !client.resumed {
		t.Fatalf("result=%+v metadataOnly=%v paused=%v", result, client.metadataOnly, client.paused)
	}
	if client.category != "未识别" || !strings.HasPrefix(strings.ToLower(client.categoryPath), strings.ToLower(root)) || !providerPathsEqual(client.routedPath, client.categoryPath) {
		t.Fatalf("result=%+v category=%q categoryPath=%q routedPath=%q resumed=%v", result, client.category, client.categoryPath, client.routedPath, client.resumed)
	}
	var task models.DownloadTask
	if err := queue.db.First(&task, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if task.ScrapeStatus != "completed_unrecognized" && task.ScrapeStatus != "fallback_unrecognized" {
		t.Fatalf("task=%+v", task)
	}
	if task.Phase != models.DownloadTaskStatusCompleted {
		t.Fatalf("completion phase=%q", task.Phase)
	}
	client.mu.Lock()
	submits := client.submits
	client.mu.Unlock()
	if submits != 1 {
		t.Fatalf("classification fallback resubmitted the download: submits=%d", submits)
	}
	var actionCount int64
	if err := queue.db.Model(&models.JobActionRequest{}).Where("job_id = ?", task.JobID).Count(&actionCount).Error; err != nil || actionCount != 0 {
		t.Fatalf("classification fallback created action requests: count=%d err=%v", actionCount, err)
	}
}

func TestProviderPathComparisonIsCrossPlatformAndRejectsDifferentTargets(t *testing.T) {
	tests := []struct {
		left  string
		right string
		want  bool
	}{
		{left: `D:\Staging\电影\`, right: `d:/staging/电影`, want: true},
		{left: `\\NAS\Share\Staging\电影`, right: `//nas/share/staging/电影/`, want: true},
		{left: `/srv/staging/电影/`, right: `/srv/staging/电影`, want: true},
		{left: `/srv/staging/电影`, right: `/srv/staging/other`, want: false},
		{left: `D:\Staging\电影`, right: `/srv/staging/电影`, want: false},
		{left: `relative/path`, right: `relative/path`, want: false},
	}
	for _, test := range tests {
		if got := providerPathsEqual(test.left, test.right); got != test.want {
			t.Fatalf("providerPathsEqual(%q, %q)=%v, want %v", test.left, test.right, got, test.want)
		}
	}
}

func TestRouteCategoryUpdatesExistingCategoryToTaskStagingSnapshotBeforeResume(t *testing.T) {
	downloads, _, _, _, _ := downloadFixture(t)
	root := t.TempDir()
	client := &metadataDownloadClient{stubDownloadClient: &stubDownloadClient{}, categories: []downloadpkg.Category{{Name: "电影", SavePath: filepath.Join(root, "outside")}}}
	worker := NewDownloadWorker(downloads)
	task := persistedRouteTask(t, downloads, "route-update", root)
	if err := worker.routeCategory(context.Background(), &task, client, root, "电影", "classified", "", ""); err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(root, "电影")
	if !providerPathsEqual(client.categoryPath, expected) || !providerPathsEqual(client.routedPath, expected) || !client.resumed {
		t.Fatalf("categoryPath=%q routedPath=%q resumed=%v", client.categoryPath, client.routedPath, client.resumed)
	}
	if got := strings.Join(client.categoryCalls, ","); got != "categories,update,categories,set,resume" {
		t.Fatalf("category call order=%q", got)
	}
}

func TestRouteCategoryRejectsProviderThatIgnoresCategoryUpdate(t *testing.T) {
	downloads, _, _, _, _ := downloadFixture(t)
	root := t.TempDir()
	client := &metadataDownloadClient{stubDownloadClient: &stubDownloadClient{}, categories: []downloadpkg.Category{{Name: "电影", SavePath: filepath.Join(root, "old")}}, keepCategoryPath: true}
	worker := NewDownloadWorker(downloads)
	task := models.DownloadTask{ProviderTaskID: "provider-hash", StagingAbsolutePath: root}
	if err := worker.routeCategory(context.Background(), &task, client, root, "电影", "classified", "", ""); codeOfProviderError(err) != "downloader_category_outside_staging" {
		t.Fatalf("route error=%v", err)
	}
	if client.resumed || client.routedPath != "" {
		t.Fatalf("unverified category was routed/resumed: routedPath=%q resumed=%v", client.routedPath, client.resumed)
	}
	if got := strings.Join(client.categoryCalls, ","); got != "categories,update,categories" {
		t.Fatalf("category call order=%q", got)
	}
}

func TestRouteCategoryDoesNotResumeWhenCategoryUpdateFails(t *testing.T) {
	downloads, _, _, _, _ := downloadFixture(t)
	root := t.TempDir()
	client := &metadataDownloadClient{
		stubDownloadClient: &stubDownloadClient{},
		categories:         []downloadpkg.Category{{Name: "电影", SavePath: filepath.Join(root, "old")}},
		updateCategoryErr:  downloadpkg.Error("downloader_category_update_failed", true, errors.New("provider rejected update")),
	}
	worker := NewDownloadWorker(downloads)
	task := models.DownloadTask{ProviderTaskID: "provider-hash", StagingAbsolutePath: root}
	err := worker.routeCategory(context.Background(), &task, client, root, "电影", "classified", "", "")
	code, retryable := downloadpkg.ErrorInfo(err)
	if code != "downloader_category_update_failed" || !retryable {
		t.Fatalf("code=%q retryable=%v err=%v", code, retryable, err)
	}
	if client.resumed || client.routedPath != "" {
		t.Fatalf("failed category update was routed/resumed: routedPath=%q resumed=%v", client.routedPath, client.resumed)
	}
	if got := strings.Join(client.categoryCalls, ","); got != "categories,update" {
		t.Fatalf("category call order=%q", got)
	}
}

func TestRouteCategoryRetryReusesImmutableTaskStagingSnapshot(t *testing.T) {
	downloads, _, _, _, _ := downloadFixture(t)
	root := t.TempDir()
	client := &metadataDownloadClient{
		stubDownloadClient: &stubDownloadClient{},
		categories:         []downloadpkg.Category{{Name: "电影", SavePath: filepath.Join(root, "old")}},
		updateCategoryErr:  downloadpkg.Error("downloader_category_update_failed", true, errors.New("temporary failure")),
	}
	worker := NewDownloadWorker(downloads)
	task := persistedRouteTask(t, downloads, "route-retry", root)
	if err := worker.routeCategory(context.Background(), &task, client, root, "电影", "classified", "", ""); err == nil {
		t.Fatal("first route unexpectedly succeeded")
	}
	client.updateCategoryErr = nil
	client.categoryCalls = nil
	if err := worker.routeCategory(context.Background(), &task, client, root, "电影", "classified", "", ""); err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(root, "电影")
	if !providerPathsEqual(client.categoryPath, expected) || !providerPathsEqual(client.routedPath, expected) || !client.resumed {
		t.Fatalf("categoryPath=%q routedPath=%q resumed=%v", client.categoryPath, client.routedPath, client.resumed)
	}
	if got := strings.Join(client.categoryCalls, ","); got != "categories,update,categories,set,resume" {
		t.Fatalf("retry call order=%q", got)
	}
}

func persistedRouteTask(t *testing.T, downloads *DownloadService, id, root string) models.DownloadTask {
	t.Helper()
	task := models.DownloadTask{ID: id, OwnerID: 1, ProviderTaskID: "provider-hash", StagingAbsolutePath: root, Phase: models.DownloadTaskStatusClassifying, SourceCiphertext: "encrypted", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	_, err := downloads.queue.EnqueueWith(EnqueueJobInput{OwnerID: task.OwnerID, JobType: "download", DisplayName: id, Payload: downloadJobPayload{DownloadTaskID: id, ResourceAccessVersion: 1}}, func(tx *gorm.DB, job models.Job) error {
		task.JobID = job.ID
		return tx.Create(&task).Error
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestRouteCategoryRejectsResolvedPathOutsideTaskSnapshot(t *testing.T) {
	downloads, _, _, _, _ := downloadFixture(t)
	snapshot := t.TempDir()
	different := t.TempDir()
	client := &metadataDownloadClient{stubDownloadClient: &stubDownloadClient{}}
	worker := NewDownloadWorker(downloads)
	task := models.DownloadTask{ProviderTaskID: "provider-hash", StagingAbsolutePath: snapshot}
	if err := worker.routeCategory(context.Background(), &task, client, different, "电影", "classified", "", ""); codeOfProviderError(err) != "downloader_category_outside_staging" {
		t.Fatalf("route error=%v", err)
	}
	if len(client.categoryCalls) != 0 || client.resumed {
		t.Fatalf("outside snapshot reached provider: calls=%v resumed=%v", client.categoryCalls, client.resumed)
	}
}

func TestRouteCategoryRejectsMissingTaskStagingSnapshot(t *testing.T) {
	downloads, _, _, _, _ := downloadFixture(t)
	root := t.TempDir()
	client := &metadataDownloadClient{stubDownloadClient: &stubDownloadClient{}}
	worker := NewDownloadWorker(downloads)
	task := models.DownloadTask{ProviderTaskID: "provider-hash"}
	if err := worker.routeCategory(context.Background(), &task, client, root, "电影", "classified", "", ""); codeOfProviderError(err) != "downloader_category_outside_staging" {
		t.Fatalf("route error=%v", err)
	}
	if len(client.categoryCalls) != 0 || client.resumed {
		t.Fatalf("missing snapshot reached provider: calls=%v resumed=%v", client.categoryCalls, client.resumed)
	}
}

func TestDownloadWorkerLoadPromotesValidatedLegacyStagingSnapshotForStrictRouting(t *testing.T) {
	downloads, downloaders, queue, actor, _ := downloadFixture(t)
	metadataClient := &metadataDownloadClient{stubDownloadClient: &stubDownloadClient{}}
	registry := downloadpkg.NewRegistry()
	if err := registry.Register(models.DownloaderTypeQBittorrent, downloadpkg.Capabilities{}, func(downloadpkg.Config) (downloadpkg.Client, error) { return metadataClient, nil }); err != nil {
		t.Fatal(err)
	}
	downloaders.registry = registry
	root := t.TempDir()
	storage := models.Storage{Name: "Legacy staging", NameNormalized: "legacy staging", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	configureDownloadStaging(t, queue, storage.ID)
	provider, err := downloaders.Create(actor, DownloaderInput{Name: "Legacy qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:legacy-staging"}}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var job models.Job
	if err := queue.db.First(&job, "id = ?", created.JobID).Error; err != nil {
		t.Fatal(err)
	}
	worker := NewDownloadWorker(downloads)
	claimed := ClaimedJob{Job: job}
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", created.ID).Updates(map[string]any{"staging_absolute_path": "", "staging_storage_id": nil, "staging_relative_path": "/"}).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, err := worker.load(context.Background(), claimed); ErrorCode(err) != CodeDownloadStagingRequired {
		t.Fatalf("missing absolute and legacy snapshot error=%v code=%q", err, ErrorCode(err))
	}
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", created.ID).Updates(map[string]any{"staging_storage_id": storage.ID}).Error; err != nil {
		t.Fatal(err)
	}
	task, _, client, _, savePath, err := worker.load(context.Background(), claimed)
	if err != nil {
		t.Fatal(err)
	}
	if !providerPathsEqual(task.StagingAbsolutePath, root) || !providerPathsEqual(savePath, root) {
		t.Fatalf("promoted=%q savePath=%q root=%q", task.StagingAbsolutePath, savePath, root)
	}
	var persisted models.DownloadTask
	if err := queue.db.First(&persisted, "id = ?", created.ID).Error; err != nil || persisted.StagingAbsolutePath != "" {
		t.Fatalf("worker read persisted promoted path=%q err=%v", persisted.StagingAbsolutePath, err)
	}
	task.ProviderTaskID = "provider-hash"
	if err := worker.routeCategory(context.Background(), &task, client.(downloadpkg.MetadataClient), savePath, "电影", "classified", "", ""); err != nil {
		t.Fatal(err)
	}
	if !metadataClient.resumed || !providerPathsEqual(metadataClient.routedPath, filepath.Join(root, "电影")) {
		t.Fatalf("resumed=%v routedPath=%q", metadataClient.resumed, metadataClient.routedPath)
	}
}

func TestProviderRoutingFailureDoesNotPersistClassifiedState(t *testing.T) {
	downloads, downloaders, queue, actor, _ := downloadFixture(t)
	root := t.TempDir()
	storage := models.Storage{Name: "Routing staging", NameNormalized: "routing staging", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	configureDownloadStaging(t, queue, storage.ID)
	provider, err := downloaders.Create(actor, DownloaderInput{Name: "Routing qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:routing"}}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", created.ID).Updates(map[string]any{"provider_task_id": "provider-hash", "phase": models.DownloadTaskStatusClassifying}).Error; err != nil {
		t.Fatal(err)
	}
	var task models.DownloadTask
	if err := queue.db.First(&task, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	worker := NewDownloadWorker(downloads)
	confidence, tmdbID := .98, int64(550)
	match := scrapeMatch{Title: "Example", MediaType: "movie", Category: "电影", TMDBID: &tmdbID, Confidence: &confidence, Confident: true}
	if err := worker.persistScrape(&task, match, "matched", 1); err != nil {
		t.Fatal(err)
	}
	client := &metadataDownloadClient{stubDownloadClient: &stubDownloadClient{}, setCategoryErr: downloadpkg.Error("downloader_unavailable", true, errors.New("offline"))}
	if err := worker.routeCategory(context.Background(), &task, client, root, match.Category, "classified", "", ""); codeOfProviderError(err) != "downloader_unavailable" {
		t.Fatalf("provider routing failure was not preserved: %v", err)
	}
	var persisted models.DownloadTask
	if err := queue.db.First(&persisted, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.ScrapeStatus != "matched" || persisted.Phase != models.DownloadTaskStatusClassifying || persisted.ProviderTaskID != task.ProviderTaskID || persisted.IdentitySnapshotJSON != task.IdentitySnapshotJSON || persisted.IdentityRevision != task.IdentityRevision || client.resumed || client.submits != 0 {
		t.Fatalf("routing failure skipped retry state: status=%q resumed=%v", persisted.ScrapeStatus, client.resumed)
	}
	if got := strings.Join(client.categoryCalls, ","); got != "categories,create,categories,set" {
		t.Fatalf("failed provider route call order=%q", got)
	}
}

func codeOfProviderError(err error) string {
	code, _ := downloadpkg.ErrorInfo(err)
	return code
}

func TestSchedulerReconcilesWaitingDownloadCancelBeforeFinalJobState(t *testing.T) {
	for _, test := range []struct {
		name           string
		cancelErr      error
		wantJobStatus  string
		wantTaskStatus string
	}{
		{name: "provider available", wantJobStatus: models.JobStatusCancelled, wantTaskStatus: models.DownloadTaskStatusCancelled},
		{name: "provider unavailable", cancelErr: downloadpkg.Error("downloader_unavailable", true, errors.New("offline")), wantJobStatus: models.JobStatusFailed, wantTaskStatus: models.DownloadTaskStatusWaiting},
	} {
		t.Run(test.name, func(t *testing.T) {
			downloads, downloaders, queue, actor, client := downloadFixture(t)
			client.cancelErr = test.cancelErr
			root := t.TempDir()
			storage := models.Storage{Name: "Control staging " + test.name, NameNormalized: "control staging " + test.name, Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
			if err := queue.db.Create(&storage).Error; err != nil {
				t.Fatal(err)
			}
			configureDownloadStaging(t, queue, storage.ID)
			provider, err := downloaders.Create(actor, DownloaderInput{Name: "Control qBit " + test.name, Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Enabled: true}, RequestContext{})
			if err != nil {
				t.Fatal(err)
			}
			created, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:control"}}, RequestContext{})
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := queue.Claim([]string{"download"})
			if err != nil || claimed == nil {
				t.Fatalf("claim=%+v err=%v", claimed, err)
			}
			if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", created.ID).Updates(map[string]any{"provider_task_id": "provider-hash", "phase": models.DownloadTaskStatusWaiting}).Error; err != nil {
				t.Fatal(err)
			}
			if err := queue.Fail(claimed.Job.ID, claimed.LeaseToken, "test_failure", "测试失败"); err != nil {
				t.Fatal(err)
			}
			controlled, err := queue.Control(actor, claimed.Job.ID, "cancel", RequestContext{})
			if err != nil {
				t.Fatal(err)
			}
			if controlled.Status != models.JobStatusQueued || controlled.InterruptPending != models.JobStatusCancelled || controlled.Action != nil || client.cancelled {
				t.Fatalf("control=%+v providerCalled=%v", controlled, client.cancelled)
			}
			worker := NewDownloadWorker(downloads)
			worker.pollInterval = time.Millisecond
			registry := NewWorkerRegistry()
			if err := registry.Register("download", worker); err != nil {
				t.Fatal(err)
			}
			scheduler := NewScheduler(queue, registry, zerolog.Nop())
			scheduler.tick = 100 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			if err := scheduler.Start(ctx); err != nil {
				cancel()
				t.Fatal(err)
			}
			defer func() { cancel(); scheduler.Close() }()
			queue.wake()
			deadline := time.Now().Add(3 * time.Second)
			var final JobDTO
			for time.Now().Before(deadline) {
				final, err = queue.Get(actor, claimed.Job.ID)
				if err == nil && final.Status == test.wantJobStatus && final.InterruptPending == "" {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			client.mu.Lock()
			providerCancelled, deleteData := client.cancelled, client.deleteData
			client.mu.Unlock()
			if final.Status != test.wantJobStatus || !providerCancelled || deleteData || final.InterruptPending != "" || final.CancellationRequested {
				var attempts []models.JobAttempt
				_ = queue.db.Where("job_id = ?", claimed.Job.ID).Order("attempt_number").Find(&attempts).Error
				t.Fatalf("final=%+v cancelled=%v deleteData=%v err=%v attempts=%+v", final, providerCancelled, deleteData, err, attempts)
			}
			var task models.DownloadTask
			taskErr := queue.db.First(&task, "id = ?", created.ID).Error
			if taskErr != nil || task.Phase != test.wantTaskStatus {
				t.Fatalf("unexpected retained pipeline state: task=%+v err=%v", task, taskErr)
			}
		})
	}
}
