package services

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/classification"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
)

func TestMediaArtifactBatchOnlyWritesFrozenEntries(t *testing.T) {
	_, db, _, storage, profile := mediaLibraryTestService(t)
	now := time.Now().UTC()
	library := models.MediaLibrary{Name: "Batch", NameNormalized: "batch", StorageID: storage.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision, RelativeRoot: "/", Enabled: true, MetadataArtifactsEnabled: true, DirtyGeneration: 2, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	scan := models.MediaLibraryScanRun{LibraryID: library.ID, Generation: 2, Kind: "transfer_batch", Partial: true, Status: "success", StartedAt: now}
	if err := db.Create(&scan).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		gen := uint64(1)
		if i < 12 {
			gen = 2
		}
		meta, err := marshalRecognitionMetadata(MediaRecognitionResult{Metadata: classification.Metadata{MediaType: classification.MediaTypeMovie}, Snapshot: tmdb.Snapshot{Version: 1, TMDBID: int64(i + 1), MediaType: "movie", Title: fmt.Sprintf("Movie %d", i)}})
		if err != nil {
			t.Fatal(err)
		}
		id := int64(i + 1)
		r := models.MediaLibraryRecognition{LibraryID: library.ID, SourceKey: fmt.Sprintf("%064d", i+1), InputFingerprint: strings.Repeat("a", 64), ProfileID: profile.ID, ProfileRevision: profile.Revision, Status: mediaRecognitionStatusMatched, MediaType: "movie", Title: fmt.Sprintf("Movie %d", i), TMDBID: &id, MetadataJSON: meta, LastGeneration: gen, CreatedAt: now, UpdatedAt: now}
		if err := db.Create(&r).Error; err != nil {
			t.Fatal(err)
		}
		e := models.MediaLibraryEntry{LibraryID: library.ID, RelativePath: fmt.Sprintf("/Movie%d.mkv", i), RecognitionID: &r.ID, MediaType: "movie", LastGeneration: gen, CreatedAt: now, UpdatedAt: now}
		if err := db.Create(&e).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&models.MediaArtifactRun{ID: "unrelated-old", LibraryID: library.ID, Generation: 1, PolicyJSON: "{}", Status: models.MediaArtifactStatusCompleted, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	old := models.MediaArtifact{LibraryID: library.ID, RunID: "unrelated-old", TargetKind: models.MediaArtifactTargetLocalAdjacent, RelativePath: "/Untouched.nfo", Active: true, Status: models.MediaArtifactStatusCompleted, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	queue := NewQueueService(db, NewAuditService(db))
	svc := NewMediaArtifactService(db, queue, nil, zerolog.Nop())
	if err := svc.ScheduleGeneration(library.ID, 2); err != nil {
		t.Fatal(err)
	}
	var run models.MediaArtifactRun
	if err := db.Where("library_id = ? AND generation = 2", library.ID).First(&run).Error; err != nil {
		t.Fatal(err)
	}
	var policy mediaArtifactPolicy
	if err := json.Unmarshal([]byte(run.PolicyJSON), &policy); err != nil {
		t.Fatal(err)
	}
	if len(policy.EntryIDs) != 12 || len(policy.RecognitionIDs) != 12 || policy.ScopeVersion != 1 || policy.CleanupEligible {
		t.Fatalf("invalid batch: %+v", policy)
	}
	// Later writes to generation cannot enlarge the frozen run.
	if err := db.Model(&models.MediaLibraryEntry{}).Where("library_id = ? AND last_generation = 1", library.ID).Update("last_generation", 2).Error; err != nil {
		t.Fatal(err)
	}
	// A later disjoint batch does not supersede this frozen work.
	if err := db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Updates(map[string]any{"artifact_generation": 3, "artifact_status": models.MediaArtifactStatusQueued}).Error; err != nil {
		t.Fatal(err)
	}
	claimed, err := queue.Claim([]string{JobTypeMediaArtifact})
	if err != nil || claimed == nil {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	var broadQueries []string
	callback := "test:bounded-artifact-queries"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(q *gorm.DB) {
		switch q.Statement.Table {
		case "media_library_entries", "media_library_recognitions", "media_library_source_assets":
			if !strings.Contains(q.Statement.SQL.String(), "id IN") {
				broadQueries = append(broadQueries, q.Statement.SQL.String())
			}
		case "media_artifacts":
			if !strings.Contains(q.Statement.SQL.String(), "relative_path =") && !strings.Contains(q.Statement.SQL.String(), "`id` =") {
				broadQueries = append(broadQueries, q.Statement.SQL.String())
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	result := NewMediaArtifactWorker(svc).Run(context.Background(), &providerWakeRuntime{}, *claimed)
	_ = db.Callback().Query().Remove(callback)
	if len(broadQueries) != 0 {
		t.Fatalf("batch performed broad lookups: %v", broadQueries)
	}
	if result.ErrorCode != "" {
		t.Fatalf("worker: %+v", result)
	}
	for i := 0; i < 40; i++ {
		_, err := os.Stat(filepath.Join(storage.RootPath, fmt.Sprintf("Movie%d.nfo", i)))
		if i < 12 && err != nil {
			t.Fatalf("missing batch file %d: %v", i, err)
		}
		if i >= 12 && !os.IsNotExist(err) {
			t.Fatalf("unrelated file %d touched: %v", i, err)
		}
	}
	if err := db.First(&old, old.ID).Error; err != nil || !old.Active || old.RunID != "unrelated-old" {
		t.Fatalf("unrelated manifest changed: %+v %v", old, err)
	}
	if err := db.First(&library, library.ID).Error; err != nil || library.ArtifactGeneration != 3 || library.ArtifactStatus != models.MediaArtifactStatusQueued {
		t.Fatalf("older batch overwrote newer status: %+v %v", library, err)
	}
	if err := db.First(&run, "id = ?", run.ID).Error; err != nil || run.ExpectedCount != 12 {
		t.Fatalf("run=%+v %v", run, err)
	}
	if err := db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Update("relative_root", "/changed-source").Error; err != nil {
		t.Fatal(err)
	}
	if superseded, err := svc.artifactPolicySuperseded(policy); err != nil || !superseded {
		t.Fatalf("source config change escaped batch fence: %t %v", superseded, err)
	}
}

func TestMediaArtifactBatchDoesNotReuseOldGeneration(t *testing.T) {
	management, queue, _, library, _ := strmManagementFixture(t)
	svc := NewMediaArtifactService(management.db, queue, &SignedProxyService{}, zerolog.Nop())
	now := time.Now().UTC()
	oldPolicy, _ := json.Marshal(mediaArtifactPolicy{LibraryID: library.ID, Generation: library.ArtifactGeneration, ScanRunID: 91, ScanKind: "event", ScanPartial: true})
	old := models.MediaArtifactRun{ID: "old-incompatible", LibraryID: library.ID, Generation: library.ArtifactGeneration, PolicyJSON: string(oldPolicy), Status: models.MediaArtifactStatusCompleted, CreatedAt: now, UpdatedAt: now}
	if err := management.db.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	scan := models.MediaLibraryScanRun{LibraryID: library.ID, Generation: library.ArtifactGeneration, Kind: "transfer_batch", Partial: true, Status: "success", StartedAt: now}
	if err := management.db.Create(&scan).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.ScheduleGeneration(library.ID, library.ArtifactGeneration); err != nil {
		t.Fatal(err)
	}
	var runs []models.MediaArtifactRun
	if err := management.db.Where("library_id = ?", library.ID).Find(&runs).Error; err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("old occurrence reused: %+v", runs)
	}
	if err := svc.ScheduleGeneration(library.ID, library.ArtifactGeneration); err != nil {
		t.Fatal(err)
	}
	var count int64
	management.db.Model(&models.MediaArtifactRun{}).Where("library_id = ?", library.ID).Count(&count)
	if count != 2 {
		t.Fatalf("same occurrence duplicated: %d", count)
	}
}

func TestMediaArtifactMissingBatchScopeRefusesBeforeInventory(t *testing.T) {
	management, queue, _, library, root := strmManagementFixture(t)
	svc := NewMediaArtifactService(management.db, queue, &SignedProxyService{}, zerolog.Nop())
	raw, _ := json.Marshal(mediaArtifactPolicy{LibraryID: library.ID, Generation: library.ArtifactGeneration, ScanRunID: 1, ScanKind: "event", ScanPartial: true, STRMEnabled: true, TargetKind: models.MediaArtifactTargetLocalProjection, ProjectionRoot: root})
	run := models.MediaArtifactRun{ID: "missing-batch-scope", LibraryID: library.ID, Generation: library.ArtifactGeneration, PolicyJSON: string(raw), Status: models.MediaArtifactStatusQueued}
	if err := management.db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(mediaArtifactJobPayload{ArtifactRunID: run.ID})
	inventoryReads := 0
	callback := "test:scope-refusal"
	if err := management.db.Callback().Query().After("gorm:query").Register(callback, func(q *gorm.DB) {
		switch q.Statement.Table {
		case "media_library_entries", "media_library_recognitions", "media_library_source_assets", "media_artifacts":
			inventoryReads++
		}
	}); err != nil {
		t.Fatal(err)
	}
	result := NewMediaArtifactWorker(svc).Run(context.Background(), &providerWakeRuntime{}, ClaimedJob{Job: models.Job{PayloadJSON: string(payload)}})
	_ = management.db.Callback().Query().Remove(callback)
	if result.ErrorCode == "" || inventoryReads != 0 {
		t.Fatalf("missing scope executed: %+v reads=%d", result, inventoryReads)
	}
}

func TestMediaArtifactBatchReadinessDoesNotConsumeOtherChanges(t *testing.T) {
	management, queue, _, library, _ := strmManagementFixture(t)
	svc := NewMediaArtifactService(management.db, queue, nil, zerolog.Nop())
	svc.changes = NewMediaChangeService(management.db)
	changes := []models.MediaLibraryChange{
		{LibraryID: library.ID, Revision: 1, Generation: 1, Kind: models.MediaLibraryChangeCatalog, State: models.MediaLibraryChangePending},
		{LibraryID: library.ID, Revision: 2, Generation: 2, Kind: models.MediaLibraryChangeCatalog, State: models.MediaLibraryChangePending},
		{LibraryID: library.ID, Revision: 3, Generation: 2, Kind: models.MediaLibraryChangeMetadata, State: models.MediaLibraryChangePending},
	}
	if err := management.db.Create(&changes).Error; err != nil {
		t.Fatal(err)
	}
	policy := mediaArtifactPolicy{LibraryID: library.ID, Generation: 2, ScopeVersion: 1, ChangeRevisions: []uint64{2}}
	if err := management.db.Transaction(func(tx *gorm.DB) error { _, err := svc.markBatchChangesReadyTx(tx, policy); return err }); err != nil {
		t.Fatal(err)
	}
	var rows []models.MediaLibraryChange
	if err := management.db.Where("library_id = ?", library.ID).Order("revision").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].State != models.MediaLibraryChangePending || rows[1].State != models.MediaLibraryChangeReady || rows[2].State != models.MediaLibraryChangePending {
		t.Fatalf("batch consumed unrelated changes: %+v", rows)
	}
	var count int64
	management.db.Model(&models.MediaChangePendingCleanup{}).Where("library_id = ?", library.ID).Count(&count)
	if count != 0 {
		t.Fatal("batch authorized pending cleanup")
	}
}

func TestMediaArtifactBatchFollowupSurvivesEnqueueFailure(t *testing.T) {
	management, queue, _, library, _ := strmManagementFixture(t)
	svc := NewMediaArtifactService(management.db, nil, &SignedProxyService{}, zerolog.Nop())
	libraries := NewMediaLibraryService(management.db, NewAuditService(management.db), zerolog.Nop())
	defer libraries.Close()
	libraries.artifacts = svc
	entry := models.MediaLibraryEntry{LibraryID: library.ID, RelativePath: "/Batch.mkv", ProviderID: "batch", LastGeneration: 2}
	if err := management.db.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	scan := models.MediaLibraryScanRun{LibraryID: library.ID, Generation: 2, Kind: "event", Status: "success", Partial: true, CheckpointJSON: "{}"}
	if err := management.db.Transaction(func(tx *gorm.DB) error {
		if err := freezeBatchArtifactCheckpointTx(tx, &scan); err != nil {
			return err
		}
		return tx.Create(&scan).Error
	}); err != nil {
		t.Fatal(err)
	}
	if err := libraries.recoverBatchArtifactFollowups(context.Background(), library.ID); err == nil {
		t.Fatal("enqueue failure acknowledged")
	}
	var current models.MediaLibraryScanRun
	if err := management.db.First(&current, scan.ID).Error; err != nil {
		t.Fatal(err)
	}
	var checkpoint batchArtifactCheckpoint
	_ = json.Unmarshal([]byte(current.CheckpointJSON), &checkpoint)
	if !checkpoint.Pending {
		t.Fatal("lost durable followup")
	}
	if err := management.db.Model(&entry).Update("last_generation", 3).Error; err != nil {
		t.Fatal(err)
	}
	unrelated := models.MediaLibraryEntry{LibraryID: library.ID, RelativePath: "/Unrelated.mkv", ProviderID: "other", LastGeneration: 2}
	if err := management.db.Create(&unrelated).Error; err != nil {
		t.Fatal(err)
	}
	if err := management.db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Update("artifact_generation", 3).Error; err != nil {
		t.Fatal(err)
	}
	svc.queue = queue
	if err := libraries.recoverBatchArtifactFollowups(context.Background(), library.ID); err != nil {
		t.Fatal(err)
	}
	var run models.MediaArtifactRun
	if err := management.db.Where("library_id = ?", library.ID).First(&run).Error; err != nil {
		t.Fatal(err)
	}
	var policy mediaArtifactPolicy
	_ = json.Unmarshal([]byte(run.PolicyJSON), &policy)
	if len(policy.EntryIDs) != 1 || policy.EntryIDs[0] != entry.ID {
		t.Fatalf("durable scope expanded/lost: %+v", policy)
	}
	if err := management.db.First(&current, scan.ID).Error; err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(current.CheckpointJSON), &checkpoint)
	if checkpoint.Pending {
		t.Fatal("successful retry not acknowledged")
	}
	if err := management.db.First(&library, library.ID).Error; err != nil || library.ArtifactGeneration != 3 {
		t.Fatalf("older followup rewound generation: %+v %v", library, err)
	}
	if err := libraries.recoverBatchArtifactFollowups(context.Background(), library.ID); err != nil {
		t.Fatal(err)
	}
	var count int64
	management.db.Model(&models.MediaArtifactRun{}).Where("library_id = ?", library.ID).Count(&count)
	if count != 1 {
		t.Fatal("followup replay duplicated artifact task")
	}
}
