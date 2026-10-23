//go:build quark_live

package services

// This acceptance-only assembler is absent from ordinary product builds. It
// receives an already bounded real driver factory from the owning handler;
// it never reads source credential stores or submits cloud mutations.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	storagefs "github.com/yuanjing-hash/OhMyCine-Server/internal/storage"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/updater"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"gorm.io/gorm"
)

type QuarkObservationLiveInput struct {
	StateDir     string
	Credentials  json.RawMessage
	Registry     *cloud.Registry
	Root         cloud.Item
	RootPath     string
	Sample       cloud.Item
	BaseURL      string
	RequestCount func() int // Actual transport count owned by the caller's guard.
}

type QuarkObservationLiveReport struct {
	Version                  int    `json:"version"`
	Stage                    string `json:"stage"`
	FirstCoverageComplete    bool   `json:"first_coverage_complete"`
	CatalogEntries           int    `json:"catalog_entries"`
	ObservationPages         int64  `json:"observation_pages"`
	PendingFollowupRecovered bool   `json:"pending_followup_recovered"`
	SameDBRestart            bool   `json:"same_db_restart"`
	RestartBeforeArtifacts   bool   `json:"restart_before_artifacts"`
	NoopUnchanged            bool   `json:"noop_unchanged"`
	NoopObservationPages     int64  `json:"noop_observation_pages"`
	NoopRequests             int    `json:"noop_requests"`
	STRMCount                int    `json:"strm_count"`
	STRMSHA256               string `json:"strm_sha256,omitempty"`
	ReadyNotifications       int    `json:"ready_notifications"`
	Requests                 int    `json:"requests"`
	LocalNameMetadataOnly    bool   `json:"local_name_metadata_only"`
	FixtureConnectionStorage bool   `json:"fixture_connection_storage"`
	AutomaticTimingTested    bool   `json:"automatic_timing_tested"`
	PlaybackTested           bool   `json:"playback_tested"`
}

func quarkObservationLiveInputCheck(in QuarkObservationLiveInput) error {
	return quarkObservationLiveInputModeCheck(in, true)
}

func quarkObservationLiveInputModeCheck(in QuarkObservationLiveInput, fresh bool) error {
	u, err := url.Parse(in.BaseURL)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" || (u.Scheme != "http" && u.Scheme != "https") || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() || u.Port() == "" {
		return errors.New("invalid_loopback_origin")
	}
	bundle, err := quark.ParseCredentials(in.Credentials)
	if err != nil || bundle.TV == nil || !bundle.TV.IdentityVerified || bundle.TV.RefreshToken != "" || bundle.Web != nil || bundle.Agent != nil && bundle.Agent.RefreshToken != "" {
		return errors.New("access_only_credentials_required")
	}
	if in.Registry == nil || in.RequestCount == nil || !filepath.IsAbs(in.StateDir) || !in.Root.IsDir || in.Root.ID == "" || in.Sample.IsDir || in.Sample.ID == "" || in.Sample.ParentID != in.Root.ID || in.Sample.Size <= 0 || in.Sample.Size > 64<<10 || filepath.Base(in.Sample.Name) != in.Sample.Name || strings.ContainsAny(in.Sample.Name, "\\\x00\r\n") || !strings.EqualFold(filepath.Ext(in.Sample.Name), ".mp4") {
		return errors.New("invalid_scope")
	}
	if _, err := observationRootPath(in.RootPath); err != nil {
		return errors.New("invalid_scope")
	}
	for p := filepath.Clean(in.StateDir); ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil || !info.IsDir() || storagefs.IsReparsePoint(p, info) {
			return errors.New("invalid_private_directory")
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	if fresh {
		entries, err := os.ReadDir(in.StateDir)
		if err != nil || len(entries) != 0 {
			return errors.New("new_private_directory_required")
		}
	} else {
		for _, name := range []string{"observation.db", "validation.key"} {
			p := filepath.Join(in.StateDir, name)
			info, err := os.Lstat(p)
			if err != nil || !info.Mode().IsRegular() || storagefs.IsReparsePoint(p, info) {
				return errors.New("existing_private_state_required")
			}
		}
	}
	return nil
}

type quarkObservationLiveRuntime struct {
	db, readDB *gorm.DB
	library    *MediaLibraryService
	observer   *ProviderObservationService
	artifacts  *MediaArtifactService
	changes    *MediaChangeService
	queue      *QueueService
	catalog    *CatalogSnapshotStore
	actor      Actor
	row        models.MediaLibrary
	ready      int
}

func (r *quarkObservationLiveRuntime) close() error {
	if r == nil {
		return nil
	}
	if r.library != nil {
		r.library.Close()
	}
	var first error
	for _, db := range []*gorm.DB{r.readDB, r.db} {
		if db != nil {
			pool, err := db.DB()
			if err == nil {
				err = pool.Close()
			}
			if first == nil {
				first = err
			}
		}
	}
	r.readDB, r.db = nil, nil
	return first
}

func quarkObservationLiveOpen(ctx context.Context, in QuarkObservationLiveInput, fresh bool) (_ *quarkObservationLiveRuntime, returnedError error) {
	r := &quarkObservationLiveRuntime{}
	defer func() {
		if returnedError != nil {
			_ = r.close()
		}
	}()
	path := filepath.Join(in.StateDir, "observation.db")
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	compatibility, err := updater.CheckStartupCompatibility(path, in.StateDir, executable, nil)
	if err != nil {
		return nil, err
	}
	if err := compatibility.ReserveFreshDatabase(); err != nil {
		return nil, err
	}
	r.db, err = database.Open(path)
	if err != nil {
		return nil, err
	}
	format, err := database.ReadCatalogFormat(ctx, r.db)
	if err != nil {
		return nil, err
	}
	if err := compatibility.ValidateDatabaseFormat(format); err != nil {
		return nil, err
	}
	if fresh {
		if err = database.Migrate(r.db); err != nil {
			return nil, err
		}
		if err := compatibility.CompleteInitialization(); err != nil {
			return nil, err
		}
	}
	r.readDB, err = database.OpenReadOnly(filepath.Join(in.StateDir, "observation.db"))
	if err != nil {
		return nil, err
	}
	secrets, err := credential.Open(filepath.Join(in.StateDir, "validation.key"), "")
	if err != nil {
		return nil, err
	}
	audit := NewAuditService(r.db)
	connections := NewConnectionService(r.db, audit, secrets, in.Registry, zerolog.Nop())
	r.catalog = NewCatalogSnapshotStore(r.db, r.readDB)
	r.queue = NewQueueService(r.db, audit)
	r.changes = NewMediaChangeService(r.db)
	r.changes.SetReadyHandler(func(uint, uint64) { r.ready++ })
	proxy, err := NewSignedProxyService(r.db, secrets, connections, in.BaseURL, zerolog.Nop())
	if err != nil {
		return nil, err
	}
	proxy.SetCatalogSnapshotStore(r.catalog)
	r.artifacts = NewMediaArtifactService(r.db, r.queue, proxy, zerolog.Nop())
	r.artifacts.SetCatalogSnapshotStore(r.catalog)
	r.artifacts.SetConnectionService(connections)
	r.artifacts.SetMediaChangeService(r.changes)
	r.library = NewMediaLibraryService(r.db, audit, zerolog.Nop())
	r.library.SetCatalogCompatibility(compatibility)
	r.library.SetConnectionService(connections)
	r.library.SetCatalogSnapshotStore(r.catalog)
	r.library.SetQueueService(r.queue)
	r.library.SetMediaChangeService(r.changes)
	r.library.SetArtifactService(r.artifacts)
	r.artifacts.SetCleanupService(NewSTRMManagementService(r.db, audit, r.queue, r.library, r.artifacts))
	r.library.EnableCatalogScanFollowups()
	r.observer = NewProviderObservationService(r.db)
	r.library.SetProviderObservationService(r.observer) // Real reader, publisher, fence, progress.
	var user models.User
	if fresh {
		user = models.User{Username: "isolated-observation", UsernameNormalized: "isolated-observation", DisplayName: "Isolated observation", PasswordHash: "disabled-local-fixture", Status: models.UserStatusActive}
		if err = r.db.Create(&user).Error; err != nil {
			return nil, err
		}
	} else if err = r.db.Where("username_normalized = ?", "isolated-observation").First(&user).Error; err != nil {
		return nil, err
	}
	r.actor = Actor{User: user, Permissions: map[string]struct{}{authz.PermissionMediaLibrariesCreate: {}, authz.PermissionMediaLibrariesRead: {}, authz.PermissionMediaLibrariesScan: {}}}
	if !fresh {
		if err = r.db.First(&r.row).Error; err != nil || r.row.ProviderRootID != in.Root.ID {
			return nil, errors.New("restart_scope_changed")
		}
		return r, nil
	}
	bundle, _ := quark.ParseCredentials(in.Credentials)
	connection := models.Connection{Name: "isolated-observation", NameNormalized: "isolated-observation", Provider: models.ConnectionProviderQuark, AccountID: bundle.TV.AccountID, Enabled: true, Revision: 1}
	if err = r.db.Create(&connection).Error; err != nil {
		return nil, err
	}
	sealed, err := secrets.Encrypt(connectionPurpose(connection.ID, connection.Provider), string(in.Credentials))
	if err != nil {
		return nil, err
	}
	if err = r.db.Model(&connection).Update("credential_ciphertext", sealed).Error; err != nil {
		return nil, err
	}
	storage := models.Storage{Name: "isolated-observation", NameNormalized: "isolated-observation", Type: models.StorageTypeQuark, ConnectionID: &connection.ID, RootPath: in.Root.ID, RootPathNormalized: in.Root.ID, RootDisplayPath: in.RootPath, Enabled: true, Capabilities: `{"temporary_direct_url":true,"signed_proxy":true}`}
	if err = r.db.Create(&storage).Error; err != nil {
		return nil, err
	}
	var profile models.MediaClassificationProfile
	if err = r.db.Where("code = ?", "default-v1").First(&profile).Error; err != nil {
		return nil, err
	}
	projection := filepath.Join(in.StateDir, "projection")
	if err = os.Mkdir(projection, 0700); err != nil {
		return nil, err
	}
	metadata := false
	detail, err := r.library.Create(ctx, r.actor, MediaLibraryInput{Name: "isolated-observation", StorageID: storage.ID, ProfileID: profile.ID, RelativeRoot: "/", ProviderRootID: in.Root.ID, Enabled: true, Recursive: true, STRMEnabled: true, STRMLocalRoot: projection, MetadataArtifactsEnabled: &metadata}, RequestContext{})
	if err != nil {
		return nil, err
	}
	r.row = detail.MediaLibrary
	return r, nil
}

func (r *quarkObservationLiveRuntime) round(ctx context.Context) error {
	if _, err := r.library.ScanNow(ctx, r.actor, r.row.ID); err != nil {
		return err
	}
	scope, err := r.library.ensureQuarkObservation(ctx, r.row.ID)
	if err != nil {
		return err
	}
	for steps := 0; steps < 80; steps++ {
		if err = r.observer.RunDue(ctx, 1); err != nil {
			return err
		}
		status, e := r.observer.Status(ctx, scope.ID)
		if e != nil {
			return e
		}
		if status.ErrorCode != "" {
			return errors.New("observation_failed")
		}
		if status.LastCompleteAt != nil && !status.LastCompleteAt.Before(scope.CoverageStartedAt) && status.PendingDirectories == 0 && status.PendingPublications == 0 && status.PathRepairs == 0 && status.PendingConfirmations == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return errors.New("observation_step_budget")
}

func (r *quarkObservationLiveRuntime) drain(ctx context.Context) error {
	if err := r.library.RecoverCatalogScanFollowups(ctx, 10); err != nil {
		return err
	}
	for jobs := 0; jobs < 10; jobs++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		job, err := r.queue.Claim([]string{JobTypeMediaArtifact})
		if err != nil {
			return err
		}
		if job == nil {
			for dispatches := 0; dispatches < 10; dispatches++ {
				more, err := r.changes.DispatchBatch(ctx)
				if err != nil || !more {
					return err
				}
			}
			return errors.New("notification_step_budget")
		}
		result := NewMediaArtifactWorker(r.artifacts).Run(ctx, workerRuntime{queue: r.queue, job: *job}, *job)
		if result.ErrorCode != "" || result.RetryAt != nil || result.Wait != nil {
			return errors.New("artifact_worker_incomplete")
		}
		if err = r.queue.Complete(job.Job.ID, job.LeaseToken); err != nil {
			return err
		}
	}
	return errors.New("artifact_step_budget")
}

// No dispatcher or artifact worker is started until after the database reopen.
// Assert persisted pending intent and an empty output rather than inferring the
// restart boundary from a receipt that might already have been acknowledged.
func (r *quarkObservationLiveRuntime) pendingBeforeArtifacts(ctx context.Context) error {
	var followups, jobs int64
	if err := r.db.WithContext(ctx).Model(&models.CatalogScanFollowup{}).Where("library_id = ? AND artifact_pending = ?", r.row.ID, true).Count(&followups).Error; err != nil || followups != 1 {
		return errors.New("artifact_followup_not_pending")
	}
	if err := r.db.WithContext(ctx).Model(&models.Job{}).Where("job_type = ?", JobTypeMediaArtifact).Count(&jobs).Error; err != nil || jobs != 0 {
		return errors.New("artifact_dispatch_already_started")
	}
	return filepath.WalkDir(r.row.STRMLocalRoot, func(p string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil || storagefs.IsReparsePoint(p, info) || !entry.IsDir() {
			return errors.New("artifact_output_already_exists")
		}
		return nil
	})
}

func (r *quarkObservationLiveRuntime) observationPages(ctx context.Context) (int64, error) {
	var pages int64
	err := r.db.WithContext(ctx).Model(&models.ProviderObservationBatch{}).Select("COALESCE(SUM(pages), 0)").Scan(&pages).Error
	return pages, err
}

type quarkObservationLiveSnapshot struct {
	head  models.CatalogHead
	row   models.MediaLibraryEntry
	path  string
	data  []byte
	mtime time.Time
}

func (r *quarkObservationLiveRuntime) snapshot(ctx context.Context, in QuarkObservationLiveInput, artifacts bool) (quarkObservationLiveSnapshot, error) {
	return r.snapshotAt(ctx, in, "/"+in.Sample.Name, artifacts)
}

func (r *quarkObservationLiveRuntime) snapshotAt(ctx context.Context, in QuarkObservationLiveInput, relative string, artifacts bool) (quarkObservationLiveSnapshot, error) {
	var out quarkObservationLiveSnapshot
	if err := r.db.WithContext(ctx).First(&out.head, "library_id = ?", r.row.ID).Error; err != nil {
		return out, err
	}
	var rows []models.MediaLibraryEntry
	if err := r.catalog.Read(ctx, []uint{r.row.ID}, func(reader *CatalogReader) error { return reader.Entries().Limit(2).Find(&rows).Error }); err != nil {
		return out, err
	}
	if len(rows) != 1 || rows[0].ProviderID != in.Sample.ID || rows[0].Size != in.Sample.Size || rows[0].RelativePath != relative {
		return out, errors.New("unexpected_catalog")
	}
	out.row = rows[0]
	if !artifacts {
		return out, nil
	}
	files := 0
	err := filepath.WalkDir(r.row.STRMLocalRoot, func(p string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil || storagefs.IsReparsePoint(p, info) {
			return errors.New("invalid_artifact_path")
		}
		if entry.IsDir() {
			return nil
		}
		files++
		if files > 8 {
			return errors.New("artifact_file_budget")
		}
		if strings.EqualFold(filepath.Ext(p), ".strm") {
			if out.path != "" || info.Size() < 1 || info.Size() > 16<<10 {
				return errors.New("invalid_strm_count")
			}
			out.path, out.mtime = p, info.ModTime()
			out.data, err = os.ReadFile(p)
			return err
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	u, err := url.Parse(strings.TrimSpace(string(out.data)))
	origin, _ := url.Parse(in.BaseURL)
	if err != nil || out.path == "" || u.Scheme != origin.Scheme || u.Host != origin.Host || u.User != nil || u.Query().Get("sig") == "" || u.Query().Get("exp") == "" || strings.Contains(string(out.data), in.Sample.ID) {
		return out, errors.New("invalid_signed_strm")
	}
	return out, nil
}

// RunQuarkObservationLiveValidation performs one fresh local acceptance batch.
// The owning handler must enforce cumulative API/write/body budgets in its real
// transports and hold original authorization locks. No secret enters report.
func RunQuarkObservationLiveValidation(ctx context.Context, in QuarkObservationLiveInput) (report QuarkObservationLiveReport, returnedError error) {
	report = QuarkObservationLiveReport{Version: 1, Stage: "input", LocalNameMetadataOnly: true, FixtureConnectionStorage: true}
	defer func() {
		if returnedError != nil {
			returnedError = fmt.Errorf("quark_observation_validation_%s", report.Stage)
		}
	}()
	if err := quarkObservationLiveInputCheck(in); err != nil {
		return report, err
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	startRequests := in.RequestCount()
	defer func() {
		report.Requests = in.RequestCount() - startRequests
		if report.Requests < 0 || report.Requests > 40 {
			returnedError = errors.New("request_budget")
		}
	}()
	lock, err := database.AcquireExclusiveRuntime(filepath.Join(in.StateDir, "observation.db"))
	if err != nil {
		return report, err
	}
	defer lock.Close()
	report.Stage = "initialize"
	r, err := quarkObservationLiveOpen(ctx, in, true)
	if err != nil {
		return report, err
	}
	defer func() {
		if e := r.close(); returnedError == nil {
			returnedError = e
		}
	}()
	report.Stage = "first_observation"
	if err = r.round(ctx); err != nil {
		return report, err
	}
	first, err := r.snapshot(ctx, in, false)
	if err != nil {
		return report, err
	}
	report.FirstCoverageComplete, report.CatalogEntries = true, 1
	if err = r.pendingBeforeArtifacts(ctx); err != nil {
		return report, err
	}
	report.Stage = "restart_before_artifacts"
	if err = r.close(); err != nil {
		return report, err
	}
	r, err = quarkObservationLiveOpen(ctx, in, false)
	if err != nil {
		return report, err
	}
	restored, err := r.snapshot(ctx, in, false)
	if err != nil || restored.row.ID != first.row.ID || !restored.row.CreatedAt.Equal(first.row.CreatedAt) || restored.head.Revision != first.head.Revision {
		return report, errors.New("restart_changed_identity")
	}
	report.SameDBRestart = true
	if err = r.pendingBeforeArtifacts(ctx); err != nil {
		return report, err
	}
	report.RestartBeforeArtifacts = true
	report.Stage = "artifact_recovery"
	if err = r.drain(ctx); err != nil {
		return report, err
	}
	before, err := r.snapshot(ctx, in, true)
	if err != nil || r.ready == 0 {
		return report, errors.New("artifact_recovery_incomplete")
	}
	report.PendingFollowupRecovered, report.STRMCount = true, 1
	ready := r.ready
	report.Stage = "unchanged_observation"
	beforeRequests := in.RequestCount()
	beforePages, err := r.observationPages(ctx)
	if err != nil {
		return report, err
	}
	if err = r.round(ctx); err != nil {
		return report, err
	}
	afterPages, err := r.observationPages(ctx)
	if err != nil {
		return report, err
	}
	report.NoopObservationPages, report.NoopRequests = afterPages-beforePages, in.RequestCount()-beforeRequests
	if report.NoopObservationPages < 1 || report.NoopRequests < 1 {
		return report, errors.New("noop_not_observed")
	}
	if err = r.drain(ctx); err != nil {
		return report, err
	}
	after, err := r.snapshot(ctx, in, true)
	if err != nil || before.head.Revision != after.head.Revision || before.row.ID != after.row.ID || !before.row.CreatedAt.Equal(after.row.CreatedAt) || before.path != after.path || !before.mtime.Equal(after.mtime) || !bytes.Equal(before.data, after.data) || r.ready != ready {
		return report, errors.New("noop_changed_state")
	}
	report.NoopUnchanged = true
	if report.ObservationPages, err = r.observationPages(ctx); err != nil {
		return report, err
	}
	sum := sha256.Sum256(after.data)
	report.STRMSHA256, report.ReadyNotifications = hex.EncodeToString(sum[:]), r.ready
	report.Stage = "completed"
	return report, nil
}
