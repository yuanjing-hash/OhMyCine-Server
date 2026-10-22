package services

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"gorm.io/gorm"
)

// The fixture uses the actual catalog publisher and artifact worker. Provider
// metadata/playback is synthetic, and counts prove revoked access never gets as
// far as any provider method, including when the direct-link cache is warm.
type observationRevocationDriver struct {
	*fakeCloudDriver
	scopedCalls int
	listCalls   int
}

func (d *observationRevocationDriver) ResolveScopedItem(_ context.Context, root, relative, id string) (cloud.Item, error) {
	d.scopedCalls++
	i, ok := d.items[id]
	if !ok || root != "root" || i.ParentID != root || relative != "/"+i.Name {
		return cloud.Item{}, cloud.Error(cloud.CodeNotFound, false, nil)
	}
	return i, nil
}
func (d *observationRevocationDriver) List(ctx context.Context, parent string, p cloud.PageRequest) (cloud.Page, error) {
	d.listCalls++
	return d.fakeCloudDriver.List(ctx, parent, p)
}
func (d *observationRevocationDriver) calls() int {
	return d.scopedCalls + d.listCalls + d.statCalls + int(d.directURLCalls.Load()) + d.probeCalls
}

type observationRevocationFixture struct {
	s       *MediaLibraryService
	library models.MediaLibrary
	actor   Actor
	observe observationFixture
	proxy   *SignedProxyService
	cleanup *STRMManagementService
	driver  *observationRevocationDriver
	drain   func()
}

func newObservationRevocationFixture(t *testing.T) observationRevocationFixture {
	t.Helper()
	s, library, actor := createCatalogTestLibrary(t)
	var storage models.Storage
	var profile models.MediaClassificationProfile
	if err := s.db.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.First(&profile, library.ProfileID).Error; err != nil {
		t.Fatal(err)
	}
	secrets, err := credential.Open(filepath.Join(t.TempDir(), "credential.key"), "")
	if err != nil {
		t.Fatal(err)
	}
	driver := &observationRevocationDriver{fakeCloudDriver: &fakeCloudDriver{signedProxy: true, items: map[string]cloud.Item{
		"removed":  {ID: "removed", ParentID: "root", Name: "Removed.2024.mkv", Size: 123},
		"retained": {ID: "retained", ParentID: "root", Name: "Retained.2024.mkv", Size: 123},
	}}}
	registry := cloud.NewRegistry()
	if err = registry.Register(cloud.ProviderQuark, func(cloud.Config) (cloud.Driver, error) { return driver, nil }); err != nil {
		t.Fatal(err)
	}
	connections := NewConnectionService(s.db, s.audit, secrets, registry, zerolog.Nop())
	actor.Permissions[authz.PermissionConnectionsCreate] = struct{}{}
	connection, err := connections.Create(actor, ConnectionInput{Name: "revocation-fixture", Provider: cloud.ProviderQuark, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	saveQuarkTestBundle(t, connections, connection.ID, func(c *quark.Credentials) { c.TV = testQuarkAuthorization() })
	storage.Type, storage.ConnectionID = models.StorageTypeQuark, &connection.ID
	library.ProviderRootID, library.Enabled = "root", true
	library.STRMEnabled, library.SignedProxyEnabled = true, true
	library.MetadataArtifactsEnabled = false
	library.STRMLocalRoot = t.TempDir()
	if err = s.db.Save(&storage).Error; err != nil {
		t.Fatal(err)
	}
	if err = s.db.Save(&library).Error; err != nil {
		t.Fatal(err)
	}
	if err = s.db.Transaction(func(tx *gorm.DB) error { return initializeFreshQuarkCatalogTx(tx, library, storage, profile) }); err != nil {
		t.Fatal(err)
	}
	var location struct{ File string }
	if err = s.db.Raw("PRAGMA database_list").Scan(&location).Error; err != nil {
		t.Fatal(err)
	}
	readDB, err := database.OpenReadOnly(location.File)
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := readDB.DB()
	t.Cleanup(func() { _ = pool.Close() })
	s.SetCatalogSnapshotStore(NewCatalogSnapshotStore(s.db, readDB))
	s.changes = NewMediaChangeService(s.db)
	s.queue = NewQueueService(s.db, s.audit)
	proxy, err := NewSignedProxyService(s.db, secrets, connections, "https://media.example.test", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	proxy.catalogStore = s.catalogStore
	artifacts := NewMediaArtifactService(s.db, s.queue, proxy, zerolog.Nop())
	artifacts.SetCatalogSnapshotStore(s.catalogStore)
	artifacts.SetMediaChangeService(s.changes)
	cleanup := NewSTRMManagementService(s.db, s.audit, s.queue, s, artifacts)
	artifacts.SetCleanupService(cleanup)
	s.SetArtifactService(artifacts)
	s.EnableCatalogScanFollowups()
	drain := func() {
		t.Helper()
		if err := s.RecoverCatalogScanFollowups(context.Background(), 20); err != nil {
			t.Fatal(err)
		}
		for range 20 {
			job, err := s.queue.Claim([]string{JobTypeMediaArtifact})
			if err != nil {
				t.Fatal(err)
			}
			if job == nil {
				return
			}
			if result := NewMediaArtifactWorker(artifacts).Run(context.Background(), &providerWakeRuntime{}, *job); result.ErrorCode != "" {
				t.Fatalf("artifact worker: %+v", result)
			}
			if err := s.queue.Complete(job.Job.ID, job.LeaseToken); err != nil {
				t.Fatal(err)
			}
		}
		t.Fatal("artifact queue did not converge")
	}
	clock := time.Now().UTC()
	observer := NewProviderObservationService(s.db)
	observer.now = func() time.Time { return clock }
	observer.SetFenceValidator(func(*gorm.DB, models.ProviderObservationScope) error { return nil })
	observer.SetPublisher(s)
	scope, err := observer.EnsureScope(context.Background(), ObservationScopeInput{LibraryID: library.ID, ConnectionID: connection.ID, AccountKey: "account", Provider: "quark", Kind: "library", RootID: "root", RootPath: "/", Fence: "fence"})
	if err != nil {
		t.Fatal(err)
	}
	reader := &observationTestReader{tree: map[string][]ObservationFact{"root": {obsFile("removed", "root", "Removed.2024.mkv"), obsFile("retained", "root", "Retained.2024.mkv")}}}
	f := observationFixture{db: s.db, s: observer, scope: scope, clock: &clock, reader: reader}
	f.round(t)
	f.publish(t)
	drain()
	return observationRevocationFixture{s: s, library: library, actor: actor, observe: f, proxy: proxy, cleanup: cleanup, driver: driver, drain: drain}
}

func (f observationRevocationFixture) artifact(t *testing.T, provider string) (models.MediaArtifact, string) {
	t.Helper()
	var a models.MediaArtifact
	if err := f.s.db.Where("library_id = ? AND provider_item_id = ? AND active = ? AND kind = ?", f.library.ID, provider, true, models.MediaArtifactKindSTRM).First(&a).Error; err != nil {
		var rows []struct {
			ID             uint
			ProviderItemID string
			Active         bool
			Status         string
			RunID          string
		}
		_ = f.s.db.Model(&models.MediaArtifact{}).Select("id,provider_item_id,active,status,run_id").Where("library_id = ?", f.library.ID).Find(&rows).Error
		var runs []struct {
			ID            string
			Generation    uint64
			Status        string
			CleanupStatus string
		}
		_ = f.s.db.Model(&models.MediaArtifactRun{}).Select("id,generation,status,cleanup_status").Where("library_id = ?", f.library.ID).Find(&runs).Error
		t.Logf("synthetic manifests=%+v runs=%+v", rows, runs)
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(f.library.STRMLocalRoot, filepath.FromSlash(strings.TrimPrefix(a.RelativePath, "/"))))
	if err != nil {
		t.Fatal(err)
	}
	return a, strings.TrimSpace(string(b))
}

func (f observationRevocationFixture) resolve(signed, ua string) error {
	u, err := url.Parse(signed)
	if err != nil {
		return err
	}
	exp, err := strconv.ParseInt(u.Query().Get("exp"), 10, 64)
	if err != nil {
		return err
	}
	_, err = f.proxy.Resolve(context.Background(), strings.TrimPrefix(u.Path, "/proxy/strm/"), u.Query().Get("kid"), exp, u.Query().Get("sig"), ua)
	return err
}

func (f observationRevocationFixture) missingCandidate(t *testing.T) {
	t.Helper()
	f.observe.reader.tree["root"] = []ObservationFact{obsFile("retained", "root", "Retained.2024.mkv")}
	f.observe.advance(11 * time.Minute)
	f.observe.round(t)
	f.observe.publish(t)
	if got := catalogReadEntries(t, f.s.catalogStore, f.library.ID, ""); len(got) != 2 {
		t.Fatal("first absence removed a catalog entry")
	}
	f.observe.advance(11 * time.Minute)
	f.observe.round(t)
}

func TestProviderObservationRevocationBeforeArtifactDrain(t *testing.T) {
	f := newObservationRevocationFixture(t)
	removed, oldURL := f.artifact(t, "removed")
	retained, keepURL := f.artifact(t, "retained")
	for range 2 {
		if err := f.resolve(oldURL, "Warm-STRM"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.proxy.ResolveArtifact(context.Background(), removed.OpaqueID, "Warm-Emby"); err != nil {
			t.Fatal(err)
		}
	}
	if f.driver.directURLCalls.Load() != 2 {
		t.Fatal("fixture did not warm both playback caches")
	}
	f.missingCandidate(t)
	if _, err := f.proxy.VerifyArtifactURL(oldURL); err != nil {
		t.Fatal("first absence revoked playable artifact")
	}
	f.observe.publish(t)
	if got := catalogReadEntries(t, f.s.catalogStore, f.library.ID, ""); len(got) != 1 || got[0].ProviderID != "retained" {
		t.Fatal("confirmed absence did not precisely remove one catalog entry")
	}
	path := filepath.Join(f.library.STRMLocalRoot, "Removed.2024.strm")
	if _, err := os.Stat(path); err != nil {
		t.Fatal("fixture must leave old physical STRM before worker drain", err)
	}
	before := f.driver.calls()
	checks := map[string]func() error{
		"verify_old_signature": func() error { _, e := f.proxy.VerifyArtifactURL(oldURL); return e },
		"warm_signed":          func() error { return f.resolve(oldURL, "Warm-STRM") },
		"cold_signed":          func() error { return f.resolve(oldURL, "Cold-STRM") },
		"warm_emby": func() error {
			_, e := f.proxy.ResolveArtifact(context.Background(), removed.OpaqueID, "Warm-Emby")
			return e
		},
		"cold_emby": func() error {
			_, e := f.proxy.ResolveArtifactForClient(context.Background(), removed.OpaqueID, "Cold-Emby", "127.0.0.1:1234")
			return e
		},
	}
	for name, check := range checks {
		t.Run(name, func(t *testing.T) {
			if err := check(); ErrorCode(err) != CodeProxyTargetUnavailable {
				t.Errorf("revoked target accepted or wrong error: %v", err)
			}
		})
	}
	if f.driver.calls() != before {
		t.Error("revoked playback reached cloud provider")
	}
	var current models.MediaArtifact
	if err := f.s.db.First(&current, removed.ID).Error; err != nil || current.Active {
		t.Errorf("revoked manifest remains active: active=%t err=%v", current.Active, err)
	}
	if _, err := f.proxy.VerifyArtifactURL(keepURL); err != nil {
		t.Fatal("other file signature changed", err)
	}
	if _, err := f.proxy.ResolveArtifact(context.Background(), retained.OpaqueID, "Retained-Emby"); err != nil {
		t.Fatal("other file playback was revoked", err)
	}
}

type observationRevocationRollback struct {
	service                   *MediaLibraryService
	reached                   bool
	artifactID                uint
	revokedInsideTransaction  bool
	recoveryProviderID        string
	recoveryInsideTransaction models.ProviderObservationItem
}

func (p *observationRevocationRollback) PublishObservation(ctx context.Context, input ObservationPublication, ack func(*gorm.DB) error) error {
	return p.service.PublishObservation(ctx, input, func(tx *gorm.DB) error {
		if err := ack(tx); err != nil {
			return err
		}
		p.reached = true
		var artifact models.MediaArtifact
		if err := tx.First(&artifact, p.artifactID).Error; err != nil {
			return err
		}
		p.revokedInsideTransaction = !artifact.Active
		if p.recoveryProviderID != "" {
			if err := tx.First(&p.recoveryInsideTransaction, "resource_id = ?", p.recoveryProviderID).Error; err != nil {
				return err
			}
		}
		return errors.New("synthetic rollback after observation acknowledgement")
	})
}

func TestProviderObservationRevocationRollbackIsAtomic(t *testing.T) {
	f := newObservationRevocationFixture(t)
	removed, oldURL := f.artifact(t, "removed")
	f.missingCandidate(t)
	var before models.CatalogHead
	if err := f.s.db.First(&before, "library_id = ?", f.library.ID).Error; err != nil {
		t.Fatal(err)
	}
	fault := &observationRevocationRollback{service: f.s, artifactID: removed.ID}
	f.observe.s.SetPublisher(fault)
	if _, err := f.observe.s.PublishPending(context.Background(), f.observe.scope.ID, 100); err == nil || !fault.reached {
		t.Fatal("fixture did not roll back after publication acknowledgement")
	}
	if !fault.revokedInsideTransaction {
		t.Fatal("rollback did not exercise an already revoked manifest inside the publication transaction")
	}
	var after models.CatalogHead
	if err := f.s.db.First(&after, "library_id = ?", f.library.ID).Error; err != nil || after.Revision != before.Revision {
		t.Fatal("failed publication changed catalog head")
	}
	if got := catalogReadEntries(t, f.s.catalogStore, f.library.ID, ""); len(got) != 2 {
		t.Fatal("failed publication removed media")
	}
	var current models.MediaArtifact
	if err := f.s.db.First(&current, removed.ID).Error; err != nil || !current.Active {
		t.Fatal("failed publication revoked artifact")
	}
	if _, err := f.proxy.VerifyArtifactURL(oldURL); err != nil {
		t.Fatal("failed publication invalidated old signed URL", err)
	}
	f.observe.s.SetPublisher(f.s)
	f.observe.publish(t)
	if _, err := f.proxy.VerifyArtifactURL(oldURL); ErrorCode(err) != CodeProxyTargetUnavailable {
		t.Fatal("recovered committed publication did not revoke old URL", err)
	}
}

func TestProviderObservationRevocationRestoredBeforeLateCleanup(t *testing.T) {
	f := newObservationRevocationFixture(t)
	originalEntries := catalogReadEntries(t, f.s.catalogStore, f.library.ID, "")
	var original models.MediaLibraryEntry
	for _, entry := range originalEntries {
		if entry.ProviderID == "removed" {
			original = entry
		}
	}
	if original.ID == 0 {
		t.Fatal("original identity missing")
	}
	f.missingCandidate(t)
	f.observe.publish(t)
	var deletionFollowup models.CatalogScanFollowup
	if err := f.s.db.First(&deletionFollowup, "library_id = ?", f.library.ID).Error; err != nil {
		t.Fatal(err)
	}
	// Admit the original deletion worker but do not let it perform filesystem
	// operations. Admission is real no-I/O evidence, not a manually cleared lease.
	if err := f.s.RecoverCatalogScanFollowups(context.Background(), 20); err != nil {
		t.Fatal(err)
	}
	var deletionRun models.MediaArtifactRun
	if err := f.s.db.First(&deletionRun, "library_id = ? AND generation = ?", f.library.ID, deletionFollowup.ArtifactGeneration).Error; err != nil || deletionRun.Status != models.MediaArtifactStatusQueued {
		t.Fatalf("deletion worker was not queued: %+v %v", deletionRun, err)
	}
	f.observe.reader.tree["root"] = []ObservationFact{obsFile("removed", "root", "Removed.2024.mkv"), obsFile("retained", "root", "Retained.2024.mkv")}
	f.observe.advance(time.Minute)
	f.observe.round(t)
	f.observe.publish(t)
	visibleAfterRestore := catalogReadEntries(t, f.s.catalogStore, f.library.ID, "")
	if len(visibleAfterRestore) != 2 {
		t.Fatalf("identical restored file was not republished before artifacts: entries=%+v", visibleAfterRestore)
	}
	f.drain()
	restored, signed := f.artifact(t, "removed")
	entries := catalogReadEntries(t, f.s.catalogStore, f.library.ID, "")
	found := false
	for _, entry := range entries {
		if entry.ProviderID == "removed" {
			found = entry.ID == original.ID && entry.CreatedAt.Equal(original.CreatedAt)
		}
	}
	if !found {
		t.Fatal("restoration changed stable entry or ingestion time")
	}
	path := filepath.Join(f.library.STRMLocalRoot, "Removed.2024.strm")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	cloudBefore, removedFiles := f.driver.calls(), 0
	f.cleanup.removeFile = func(p string) error { removedFiles++; return os.Remove(p) }
	late := f.cleanup.AutoCleanup(context.Background(), deletionRun.ID)
	if late.Removed != 0 || removedFiles != 0 {
		t.Fatalf("late old deletion removed restored artifact: %+v", late)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("late cleanup changed restored STRM", err)
	}
	afterStat, err := os.Stat(path)
	if err != nil || !afterStat.ModTime().Equal(stat.ModTime()) {
		t.Fatal("late cleanup rewrote restored STRM", err)
	}
	var active models.MediaArtifact
	if err = f.s.db.First(&active, restored.ID).Error; err != nil || !active.Active {
		t.Fatal("late cleanup revoked restored manifest", err)
	}
	if _, err = f.proxy.VerifyArtifactURL(signed); err != nil {
		t.Fatal("restored signature invalid", err)
	}
	if f.driver.calls() != cloudBefore {
		t.Fatal("late cleanup read cloud")
	}
	var beforeNoOp models.CatalogHead
	if err = f.s.db.First(&beforeNoOp, "library_id = ?", f.library.ID).Error; err != nil {
		t.Fatal(err)
	}
	f.observe.advance(time.Minute)
	f.observe.round(t)
	f.observe.publish(t)
	f.drain()
	var afterNoOp models.CatalogHead
	if err = f.s.db.First(&afterNoOp, "library_id = ?", f.library.ID).Error; err != nil || afterNoOp.Revision != beforeNoOp.Revision {
		t.Fatal("same restored fact published repeatedly")
	}
	noOpArtifact, noOpURL := f.artifact(t, "removed")
	if noOpArtifact.ID != restored.ID || noOpURL != signed {
		t.Fatal("unchanged restored file rewrote its artifact")
	}
	noOpStat, err := os.Stat(path)
	if err != nil || !noOpStat.ModTime().Equal(stat.ModTime()) {
		t.Fatal("unchanged restored file rewrote STRM bytes", err)
	}
}

func TestProviderObservationRevocationPreservesLateManualRecognition(t *testing.T) {
	f := newObservationRevocationFixture(t)
	ctx := context.Background()
	entries := catalogReadEntries(t, f.s.catalogStore, f.library.ID, "provider_id = ?", "removed")
	if len(entries) != 1 || entries[0].RecognitionID == nil {
		t.Fatal("initial observation has no recognized entry")
	}
	original := entries[0]
	// Both real manual services run after the last positive observation. The
	// synthetic TMDB endpoint is loopback only; no provider/network credentials
	// or production database are involved.
	recognitionSnapshotClient(t, f.s, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/movie/42" {
			_, _ = w.Write([]byte(`{"id":42,"title":"Late manual match","original_language":"zh","release_date":"2024-01-01","genres":[{"id":18}]}`))
			return
		}
		http.NotFound(w, r)
	})
	matched, err := f.s.OverrideRecognition(ctx, f.actor, f.library.ID, encodeRecognitionToken(*original.RecognitionID), MediaRecognitionOverrideInput{TMDBID: 42, MediaType: "movie"}, RequestContext{})
	if err != nil || !matched.ManualOverride || matched.Title != "Late manual match" {
		t.Fatalf("manual match was not saved: %+v %v", matched, err)
	}
	manualEntries := catalogReadEntries(t, f.s.catalogStore, f.library.ID, "provider_id = ?", "removed")
	if len(manualEntries) != 1 {
		t.Fatal("manual match lost entry")
	}
	workToken := encodeCatalogToken(manualEntries[0].WorkKey)
	document, err := f.s.CatalogMetadata(ctx, f.actor, f.library.ID, workToken)
	if err != nil {
		t.Fatal(err)
	}
	document.Editable.Title = "Latest local editorial title"
	document.Editable.Overview = "Latest local editorial overview"
	edited, err := f.s.UpdateCatalogMetadata(ctx, f.actor, f.library.ID, workToken, MediaMetadataUpdateInput{Revision: document.Revision, Editable: document.Editable}, RequestContext{})
	if err != nil || !edited.ManualOverride || edited.Editable.Title != document.Editable.Title {
		t.Fatalf("latest manual metadata was not saved: %+v %v", edited, err)
	}
	f.drain()
	var latest models.MediaLibraryRecognition
	if err := f.s.catalogStore.Read(ctx, []uint{f.library.ID}, func(r *CatalogReader) error {
		return r.Recognitions().First(&latest, *original.RecognitionID).Error
	}); err != nil {
		t.Fatal(err)
	}
	if !latest.ManualOverride || latest.TMDBID == nil || *latest.TMDBID != 42 || latest.Title != document.Editable.Title {
		t.Fatal("latest catalog manual authority missing before absence")
	}
	f.missingCandidate(t)
	// Capture is part of the same publication transaction as the head switch
	// and revocation. An error after acknowledgement must restore the old
	// recovery envelope too, even though the newest manual facts were captured
	// inside that transaction.
	var recoveryBefore models.ProviderObservationItem
	if err := f.s.db.First(&recoveryBefore, "resource_id = ?", "removed").Error; err != nil {
		t.Fatal(err)
	}
	active, _ := f.artifact(t, "removed")
	fault := &observationRevocationRollback{service: f.s, artifactID: active.ID, recoveryProviderID: "removed"}
	f.observe.s.SetPublisher(fault)
	if _, err := f.observe.s.PublishPending(ctx, f.observe.scope.ID, 100); err == nil || !fault.reached || !fault.revokedInsideTransaction {
		t.Fatal("manual recovery fixture did not roll back the revocation transaction")
	}
	var captured observationRecoveryRecognition
	if err := json.Unmarshal([]byte(fault.recoveryInsideTransaction.RecoveryRecognitionJSON), &captured); err != nil || !captured.Recognition.ManualOverride || captured.Recognition.Title != latest.Title || captured.MetadataJSON != latest.MetadataJSON {
		t.Errorf("removal transaction did not capture latest manual metadata: title=%q manual=%t err=%v", captured.Recognition.Title, captured.Recognition.ManualOverride, err)
	}
	var recoveryAfter models.ProviderObservationItem
	if err := f.s.db.First(&recoveryAfter, "resource_id = ?", "removed").Error; err != nil {
		t.Fatal(err)
	}
	if recoveryAfter.RecoveryEntryJSON != recoveryBefore.RecoveryEntryJSON || recoveryAfter.RecoveryRecognitionJSON != recoveryBefore.RecoveryRecognitionJSON || recoveryAfter.CatalogSourceEpoch != recoveryBefore.CatalogSourceEpoch || recoveryAfter.DownlistedAt == nil || recoveryBefore.DownlistedAt == nil || !recoveryAfter.DownlistedAt.Equal(*recoveryBefore.DownlistedAt) {
		t.Fatal("rolled back publication changed the durable recovery envelope")
	}
	f.observe.s.SetPublisher(f.s)
	f.observe.publish(t)
	if got := catalogReadEntries(t, f.s.catalogStore, f.library.ID, "provider_id = ?", "removed"); len(got) != 0 {
		t.Fatal("confirmed absence did not remove the manually matched entry")
	}
	f.drain()
	f.observe.reader.tree["root"] = []ObservationFact{obsFile("removed", "root", "Removed.2024.mkv"), obsFile("retained", "root", "Retained.2024.mkv")}
	f.observe.advance(time.Minute)
	f.observe.round(t)
	f.observe.publish(t)
	f.drain()
	restored := catalogReadEntries(t, f.s.catalogStore, f.library.ID, "provider_id = ?", "removed")
	if len(restored) != 1 {
		t.Fatalf("restored entry missing: %+v", restored)
	}
	if restored[0].ID != original.ID || !restored[0].CreatedAt.Equal(original.CreatedAt) || restored[0].RecognitionID == nil || *restored[0].RecognitionID != *original.RecognitionID {
		t.Errorf("stable identity or ingestion time changed: original=%+v restored=%+v", original, restored[0])
	}
	var recognition models.MediaLibraryRecognition
	if err := f.s.catalogStore.Read(ctx, []uint{f.library.ID}, func(r *CatalogReader) error {
		return r.Recognitions().First(&recognition, *restored[0].RecognitionID).Error
	}); err != nil {
		t.Fatal(err)
	}
	if !recognition.ManualOverride || recognition.TMDBID == nil || *recognition.TMDBID != 42 || recognition.Title != latest.Title || recognition.MetadataJSON != latest.MetadataJSON {
		t.Errorf("latest manual recognition lost: manual=%t title=%q tmdb=%v expected_title=%q metadata_equal=%t", recognition.ManualOverride, recognition.Title, recognition.TMDBID, latest.Title, recognition.MetadataJSON == latest.MetadataJSON)
	}
	if restored[0].Title != latest.Title || restored[0].WorkKey != manualEntries[0].WorkKey {
		t.Errorf("restored entry reverted manual work projection: title=%q work=%q", restored[0].Title, restored[0].WorkKey)
	}
}
