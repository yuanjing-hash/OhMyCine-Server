package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
)

func boundArtifactFixture(t *testing.T) (*MediaArtifactService, *CatalogSnapshotStore, models.MediaLibrary, models.CatalogArtifactBinding, models.MediaLibraryRecognition, string) {
	t.Helper()
	store, library, rec, entries := catalogFixture(t)
	metadataJSON, err := marshalRecognitionMetadata(MediaRecognitionResult{Snapshot: tmdb.Snapshot{Version: 1, TMDBID: 42, MediaType: "tv", Title: "Bound show", OriginalTitle: "Bound show", ReleaseDate: "2024-01-01"}})
	if err != nil {
		t.Fatal(err)
	}
	rec.MetadataJSON = metadataJSON
	if err := store.writeDB.Model(&library).Updates(map[string]any{"enabled": true, "metadata_artifacts_enabled": true}).Error; err != nil {
		t.Fatal(err)
	}
	catalogConvert(t, store, library, rec, entries)
	queue := NewQueueService(store.writeDB, NewAuditService(store.writeDB))
	artifacts := NewMediaArtifactService(store.writeDB, queue, nil, zerolog.Nop())
	artifacts.SetCatalogSnapshotStore(store)
	var binding models.CatalogArtifactBinding
	if err := store.writeDB.Transaction(func(tx *gorm.DB) error {
		var err error
		binding, err = artifacts.BindCatalogGenerationTx(tx, library.ID, 7)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var storage models.Storage
	if err := store.writeDB.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	return artifacts, store, library, binding, rec, filepath.Join(storage.RootPath, filepath.FromSlash(library.RelativeRoot))
}

func TestCatalogArtifactExactSnapshotSchedulesWithoutGenerationSweep(t *testing.T) {
	artifacts, store, library, binding, _, root := boundArtifactFixture(t)
	if err := artifacts.ScheduleGeneration(library.ID, 7); err != nil {
		t.Fatal(err)
	}
	if err := artifacts.ScheduleGeneration(library.ID, 7); err != nil {
		t.Fatal(err)
	}
	var jobs []models.Job
	if err := store.writeDB.Where("job_type = ?", JobTypeMediaArtifact).Find(&jobs).Error; err != nil || len(jobs) != 1 || jobs[0].Generation != 1 {
		t.Fatalf("duplicate schedule changed actual owner: jobs=%+v err=%v", jobs, err)
	}
	claimed, err := artifacts.queue.Claim([]string{JobTypeMediaArtifact})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	result := NewMediaArtifactWorker(artifacts).Run(context.Background(), &providerWakeRuntime{}, *claimed)
	if result.ErrorCode != "" {
		t.Fatalf("worker=%+v", result)
	}
	content, err := os.ReadFile(filepath.Join(root, "Show", "tvshow.nfo"))
	if err != nil || len(content) == 0 {
		t.Fatalf("NFO=%s err=%v", content, err)
	}
	if err := store.writeDB.First(&binding, "id = ?", binding.ID).Error; err != nil {
		t.Fatal(err)
	}
	if binding.State != "completed" {
		t.Fatalf("binding=%+v", binding)
	}
	var refs int64
	store.writeDB.Model(&models.CatalogSnapshotReference{}).Where("owner_id = ?", binding.ID).Count(&refs)
	if refs != 0 {
		t.Fatalf("leaked refs=%d", refs)
	}
	var facts []models.MediaLibraryRecognition
	if err := store.Read(context.Background(), []uint{library.ID}, func(r *CatalogReader) error { return r.Recognitions().Find(&facts).Error }); err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0].LastGeneration == 7 {
		t.Fatalf("generation swept=%+v", facts)
	}
	var artifact models.MediaArtifact
	if err := store.writeDB.First(&artifact, "library_id = ?", library.ID).Error; err != nil || artifact.CatalogBindingID != binding.ID {
		t.Fatalf("artifact=%+v err=%v", artifact, err)
	}
}

func TestCatalogArtifactSourceDriftAndUnboundLegacyFailClosed(t *testing.T) {
	artifacts, store, library, binding, _, _ := boundArtifactFixture(t)
	if err := store.writeDB.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Update("relative_root", "another-source").Error; err != nil {
		t.Fatal(err)
	}
	policy := mediaArtifactPolicy{LibraryID: library.ID, Generation: 7, CatalogBindingID: binding.ID}
	err := store.writeDB.Transaction(func(tx *gorm.DB) error { return artifacts.validateArtifactBindingTx(tx, policy, nil, nil, true) })
	if !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("source drift=%v", err)
	}
	policy.CatalogBindingID = ""
	if stale, err := artifacts.artifactPolicySuperseded(policy); err != nil || !stale {
		t.Fatalf("unbound stale=%v err=%v", stale, err)
	}
}

func TestCatalogArtifactConfigurationFenceIncludesOutputPolicy(t *testing.T) {
	for _, change := range []map[string]any{{"metadata_artifacts_enabled": false}, {"strm_local_root": "changed-output-root"}, {"signed_proxy_enabled": true}} {
		t.Run(fmt.Sprint(change), func(t *testing.T) {
			artifacts, store, library, binding, _, _ := boundArtifactFixture(t)
			if err := store.writeDB.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Updates(change).Error; err != nil {
				t.Fatal(err)
			}
			err := store.writeDB.Transaction(func(tx *gorm.DB) error {
				return artifacts.validateArtifactBindingTx(tx, mediaArtifactPolicy{LibraryID: library.ID, Generation: 7, CatalogBindingID: binding.ID}, nil, nil, true)
			})
			if !errors.Is(err, ErrCatalogFence) {
				t.Fatalf("output policy drift accepted: %v", err)
			}
		})
	}
}

func TestCatalogArtifactNoopScanDirtyCounterDoesNotInvalidatePendingBinding(t *testing.T) {
	artifacts, store, library, binding, _, _ := boundArtifactFixture(t)
	if err := store.writeDB.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Update("dirty_generation", 99).Error; err != nil {
		t.Fatal(err)
	}
	err := store.writeDB.Transaction(func(tx *gorm.DB) error {
		return artifacts.validateArtifactBindingTx(tx, mediaArtifactPolicy{LibraryID: library.ID, Generation: 7, CatalogBindingID: binding.ID}, nil, nil, true)
	})
	if err != nil {
		t.Fatalf("no-op counter invalidated exact binding: %v", err)
	}
}

func TestCatalogArtifactFinalizeResumesWithoutRewriting(t *testing.T) {
	artifacts, store, library, binding, _, _ := boundArtifactFixture(t)
	if err := artifacts.ScheduleGeneration(library.ID, 7); err != nil {
		t.Fatal(err)
	}
	claimed, err := artifacts.queue.Claim([]string{JobTypeMediaArtifact})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%v", err)
	}
	var run models.MediaArtifactRun
	if err := store.writeDB.First(&run, "library_id = ?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	var policy mediaArtifactPolicy
	if err := json.Unmarshal([]byte(run.PolicyJSON), &policy); err != nil {
		t.Fatal(err)
	}
	if err := store.writeDB.Model(&binding).Updates(map[string]any{"state": "applying", "finalize_after_id": 0}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.writeDB.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Update("artifact_applied_generation", 7).Error; err != nil {
		t.Fatal(err)
	}
	// Same logical run ID from an older refresh must still be retired.
	old := models.MediaArtifact{OpaqueID: "old-bound-artifact", RunID: run.ID, LibraryID: library.ID, Kind: models.MediaArtifactKindNFO, TargetKind: policy.TargetKind, RelativePath: "/Old/tvshow.nfo", Managed: true, Active: true, Status: models.MediaArtifactStatusCompleted, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := store.writeDB.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	result := NewMediaArtifactWorker(artifacts).Run(context.Background(), &providerWakeRuntime{}, *claimed)
	if result.ErrorCode != "" {
		t.Fatalf("resume=%+v", result)
	}
	if err := store.writeDB.First(&old, old.ID).Error; err != nil {
		t.Fatal(err)
	}
	if old.Active {
		t.Fatal("older binding in same run remained active")
	}
	if err := store.writeDB.First(&run, "id = ?", run.ID).Error; err != nil {
		t.Fatal(err)
	}
	if run.WrittenCount != 0 || run.Status != models.MediaArtifactStatusCompleted {
		t.Fatalf("resume rewrote=%+v", run)
	}
}

func TestCatalogArtifactManualRecognitionBindsNewHeadInSamePublication(t *testing.T) {
	artifacts, store, library, _, rec, _ := boundArtifactFixture(t)
	service := NewMediaLibraryService(store.writeDB, NewAuditService(store.writeDB), zerolog.Nop())
	t.Cleanup(service.Close)
	service.SetCatalogSnapshotStore(store)
	service.artifacts = artifacts
	source, err := service.captureRecognitionWriteContext(library.ID)
	if err != nil {
		t.Fatal(err)
	}
	result := MediaRecognitionResult{Status: "matched", MediaType: "tv", Title: "Manual corrected", TMDBID: rec.TMDBID, Snapshot: tmdb.Snapshot{Version: 1, TMDBID: 42, MediaType: "tv", Title: "Manual corrected"}}
	if err := service.persistRecognitionResult(rec, source.Profile, result, true, source); err != nil {
		t.Fatal(err)
	}
	var bindings []models.CatalogArtifactBinding
	if err := store.writeDB.Where("library_id = ?", library.ID).Order("head_revision").Find(&bindings).Error; err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 2 || bindings[1].HeadRevision != 2 || bindings[1].Generation != 8 || bindings[1].State != "scheduled" {
		t.Fatalf("bindings=%+v", bindings)
	}
	var anchor models.MediaLibraryRecognition
	if err := store.writeDB.First(&anchor, rec.ID).Error; err != nil {
		t.Fatal(err)
	}
	if anchor.LastGeneration != 0 || anchor.Title == "Manual corrected" {
		t.Fatalf("anchor swept=%+v", anchor)
	}
}

type boundArtifactCallbackRuntime struct{ once func() }

func (r *boundArtifactCallbackRuntime) Heartbeat(*float64, *int64, *int64, *float64, *int64) error {
	if r.once != nil {
		fn := r.once
		r.once = nil
		fn()
	}
	return nil
}
func (*boundArtifactCallbackRuntime) Checkpoint(any) error { return nil }

func TestCatalogArtifactNewBindingDuringWritesCannotPublishOldReadiness(t *testing.T) {
	artifacts, store, library, old, rec, _ := boundArtifactFixture(t)
	if err := artifacts.ScheduleGeneration(library.ID, 7); err != nil {
		t.Fatal(err)
	}
	claimed, err := artifacts.queue.Claim([]string{JobTypeMediaArtifact})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%v", err)
	}
	runtime := &boundArtifactCallbackRuntime{once: func() {
		candidate, token := catalogCandidate(t, store, library, "delta", 1)
		rec.Title = "newer head"
		if err := store.AppendBatch(context.Background(), candidate.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}); err != nil {
			t.Fatal(err)
		}
		catalogPublish(t, store, candidate, token)
		if err := store.writeDB.Transaction(func(tx *gorm.DB) error { _, err := artifacts.BindCatalogGenerationTx(tx, library.ID, 7); return err }); err != nil {
			t.Fatal(err)
		}
	}}
	result := NewMediaArtifactWorker(artifacts).Run(context.Background(), runtime, *claimed)
	if result.ErrorCode != "" || result.RetryAt != nil {
		t.Fatalf("result=%+v", result)
	}
	// Superseded output is now reconciled using its exact receipts. That is
	// successful bookkeeping, not publication of the obsolete artifact binding.
	var proof models.CatalogPhysicalWrite
	if err := store.writeDB.First(&proof, "job_id=?", claimed.Job.ID).Error; err != nil || proof.State != "settled" {
		t.Fatalf("superseded physical proof=%+v err=%v", proof, err)
	}
	if err := store.writeDB.First(&library, library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if library.ArtifactAppliedGeneration == 7 {
		t.Fatal("old snapshot published applied")
	}
	if err := store.writeDB.Model(&old).Update("updated_at", time.Now().UTC().Add(-2*time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	if err := artifacts.RecoverCatalogArtifactBindings(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	if err := store.writeDB.First(&old, "id = ?", old.ID).Error; err != nil {
		t.Fatal(err)
	}
	if old.State != "superseded" {
		t.Fatalf("old binding=%+v", old)
	}
	var refs int64
	store.writeDB.Model(&models.CatalogSnapshotReference{}).Where("owner_id = ?", old.ID).Count(&refs)
	if refs != 0 {
		t.Fatalf("old refs=%d", refs)
	}
}

func TestCatalogArtifactLostLeaseDoesNotWriteOrApply(t *testing.T) {
	artifacts, store, library, binding, _, root := boundArtifactFixture(t)
	if err := artifacts.ScheduleGeneration(library.ID, 7); err != nil {
		t.Fatal(err)
	}
	claimed, err := artifacts.queue.Claim([]string{JobTypeMediaArtifact})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%v", err)
	}
	claimed.LeaseToken = "wrong-owner"
	result := NewMediaArtifactWorker(artifacts).Run(context.Background(), &providerWakeRuntime{}, *claimed)
	if result.ErrorCode == "" {
		t.Fatal("accepted lost lease")
	}
	if _, err := os.Stat(filepath.Join(root, "Show", "tvshow.nfo")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("physical write after lost lease=%v", err)
	}
	if err := store.writeDB.First(&binding, "id = ?", binding.ID).Error; err != nil {
		t.Fatal(err)
	}
	if binding.State == "completed" || binding.State == "failed" {
		t.Fatalf("invalid owner mutated binding=%+v", binding)
	}
}

func TestCatalogArtifactReservedManifestSurvivesPhysicalWriteCrash(t *testing.T) {
	artifacts, store, library, binding, _, root := boundArtifactFixture(t)
	if err := artifacts.ScheduleGeneration(library.ID, 7); err != nil {
		t.Fatal(err)
	}
	claimed, err := artifacts.queue.Claim([]string{JobTypeMediaArtifact})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%v", err)
	}
	var run models.MediaArtifactRun
	if err := store.writeDB.First(&run, "library_id = ?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	manifest := newArtifactManifestIndex(1)
	manifest.lazy = true
	manifest.catalogBindingID = binding.ID
	manifest.reserve = func(a *models.MediaArtifact) error { return store.writeDB.Create(a).Error }
	if _, err := artifacts.writeLocalArtifact(root, run, localArtifactSpec{SourceIdentity: "recognition:1", Kind: models.MediaArtifactKindNFO, TargetKind: models.MediaArtifactTargetLocalAdjacent, RelativePath: "/Show/tvshow.nfo", Manifest: manifest, Content: func(models.MediaArtifact) ([]byte, error) { return []byte("before manifest commit"), nil }}); err != nil {
		t.Fatal(err)
	}
	// Lose the in-memory completed manifest: only the prior ownership reservation
	// survives. A restarted worker must update it, not skip an unmanaged file.
	var row models.MediaArtifact
	if err := store.writeDB.First(&row, "library_id = ?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != models.MediaArtifactStatusQueued {
		t.Fatalf("reservation=%+v", row)
	}
	result := NewMediaArtifactWorker(artifacts).Run(context.Background(), &providerWakeRuntime{}, *claimed)
	if result.ErrorCode != "" {
		t.Fatalf("restart=%+v", result)
	}
	if err := store.writeDB.First(&row, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != models.MediaArtifactStatusCompleted {
		t.Fatalf("not recovered=%+v", row)
	}
}

func TestCatalogArtifactRecoveryDoesNotRestartCancelledQueueJob(t *testing.T) {
	artifacts, store, library, binding, _, _ := boundArtifactFixture(t)
	if err := artifacts.ScheduleGeneration(library.ID, 7); err != nil {
		t.Fatal(err)
	}
	var run models.MediaArtifactRun
	if err := store.writeDB.First(&run, "library_id = ?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if run.JobID == nil {
		t.Fatal("no job")
	}
	if err := store.writeDB.Model(&models.Job{}).Where("id = ?", *run.JobID).Update("status", models.JobStatusCancelled).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.writeDB.Model(&binding).Update("updated_at", time.Now().UTC().Add(-2*time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	if err := artifacts.RecoverCatalogArtifactBindings(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	var current models.MediaArtifactRun
	if err := store.writeDB.First(&current, "id = ?", run.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.PolicyJSON != run.PolicyJSON {
		t.Fatal("recovery refreshed cancelled job")
	}
	var job models.Job
	if err := store.writeDB.First(&job, "id = ?", *run.JobID).Error; err != nil {
		t.Fatal(err)
	}
	if job.Status != models.JobStatusCancelled {
		t.Fatalf("revived cancellation=%+v", job)
	}
}

func TestCatalogArtifactPostCommitScheduleFailureStillReturnsSavedRecognition(t *testing.T) {
	artifacts, store, library, _, rec, root := boundArtifactFixture(t)
	artifacts.queue = nil // deterministic enqueue/preparation fault after publication
	service := NewMediaLibraryService(store.writeDB, NewAuditService(store.writeDB), zerolog.Nop())
	t.Cleanup(service.Close)
	service.SetCatalogSnapshotStore(store)
	service.artifacts = artifacts
	source, err := service.captureRecognitionWriteContext(library.ID)
	if err != nil {
		t.Fatal(err)
	}
	result := MediaRecognitionResult{Status: "matched", MediaType: "tv", Title: "Saved despite scheduling fault", TMDBID: rec.TMDBID, Snapshot: tmdb.Snapshot{Version: 1, TMDBID: 42, MediaType: "tv", Title: "Saved despite scheduling fault"}}
	if err := service.persistRecognitionResult(rec, source.Profile, result, true, source); err != nil {
		t.Fatalf("committed save reported failure: %v", err)
	}
	var user models.User
	if err := store.writeDB.First(&user, "username = ?", "library-test").Error; err != nil {
		t.Fatal(err)
	}
	exact, err := service.Recognition(context.Background(), Actor{User: user, Permissions: map[string]struct{}{authz.PermissionMediaLibrariesRead: {}}}, library.ID, encodeRecognitionToken(rec.ID))
	if err != nil || exact.Title != result.Title || !exact.ManualOverride {
		t.Fatalf("saved GET=%+v err=%v", exact, err)
	}
	var pending models.CatalogArtifactBinding
	if err := store.writeDB.First(&pending, "library_id = ? AND head_revision = ?", library.ID, 2).Error; err != nil {
		t.Fatal(err)
	}
	if pending.State != "pending" {
		t.Fatalf("recovery receipt=%+v", pending)
	}
	if _, err := os.Stat(filepath.Join(root, "Show", "tvshow.nfo")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("save performed physical repair=%v", err)
	}
}

func TestCatalogArtifactFinalizationUsesPersistedKeysetAndPartialDoesNotPrune(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%t", partial), func(t *testing.T) {
			artifacts, store, library, binding, _, _ := boundArtifactFixture(t)
			scan := models.MediaLibraryScanRun{LibraryID: library.ID, Generation: 7, Kind: "full", Status: "success", Partial: partial, StartedAt: time.Now().UTC()}
			if err := store.writeDB.Create(&scan).Error; err != nil {
				t.Fatal(err)
			}
			if err := artifacts.ScheduleGeneration(library.ID, 7); err != nil {
				t.Fatal(err)
			}
			claimed, err := artifacts.queue.Claim([]string{JobTypeMediaArtifact})
			if err != nil || claimed == nil {
				t.Fatalf("claim=%v", err)
			}
			var run models.MediaArtifactRun
			if err := store.writeDB.First(&run, "library_id = ?", library.ID).Error; err != nil {
				t.Fatal(err)
			}
			var policy mediaArtifactPolicy
			if err := json.Unmarshal([]byte(run.PolicyJSON), &policy); err != nil {
				t.Fatal(err)
			}
			if policy.ScanPartial != partial {
				t.Fatal("scheduled policy did not freeze scan partiality")
			}
			old := make([]models.MediaArtifact, CatalogBatchRows*2+3)
			for i := range old {
				old[i] = models.MediaArtifact{OpaqueID: fmt.Sprintf("retire-%d", i), RunID: run.ID, LibraryID: library.ID, Kind: models.MediaArtifactKindNFO, TargetKind: policy.TargetKind, RelativePath: fmt.Sprintf("/Old/%d.nfo", i), Managed: true, Active: true, Status: models.MediaArtifactStatusCompleted}
			}
			if err := store.writeDB.CreateInBatches(&old, CatalogBatchRows).Error; err != nil {
				t.Fatal(err)
			}
			// Simulate a prior committed finalization page, followed by a process exit.
			cursor := uint(0)
			if !partial {
				cursor = old[CatalogBatchRows-1].ID
				if err := store.writeDB.Model(&models.MediaArtifact{}).Where("id <= ?", cursor).Update("active", false).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := store.writeDB.Model(&binding).Updates(map[string]any{"state": "applying", "finalize_after_id": cursor}).Error; err != nil {
				t.Fatal(err)
			}
			if err := store.writeDB.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Update("artifact_applied_generation", 7).Error; err != nil {
				t.Fatal(err)
			}
			result := NewMediaArtifactWorker(artifacts).Run(context.Background(), &providerWakeRuntime{}, *claimed)
			if result.ErrorCode != "" {
				t.Fatalf("result=%+v", result)
			}
			var active int64
			if err := store.writeDB.Model(&models.MediaArtifact{}).Where("library_id = ? AND active = ?", library.ID, true).Count(&active).Error; err != nil {
				t.Fatal(err)
			}
			if (!partial && active != 0) || (partial && active != int64(len(old))) {
				t.Fatalf("partial=%t active=%d", partial, active)
			}
			if err := store.writeDB.First(&binding, "id = ?", binding.ID).Error; err != nil {
				t.Fatal(err)
			}
			if binding.State != "completed" || (!partial && binding.FinalizeAfterID != old[len(old)-1].ID) {
				t.Fatalf("cursor=%+v", binding)
			}
		})
	}
}
