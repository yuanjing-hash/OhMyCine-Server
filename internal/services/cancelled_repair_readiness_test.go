package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestCancelledUnenteredRepairReleasesDiagnosisWithoutErasingAdmission(t *testing.T) {
	s, db, actor, storage, profile := mediaLibraryTestService(t)
	actor.Permissions[authz.PermissionJobsControlAll] = struct{}{}
	actor.Permissions[authz.PermissionJobsReadAll] = struct{}{}
	library, err := s.Create(context.Background(), actor, testLibraryInput("cancel unentered", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.MediaLibrary{}).Where("id=?", library.ID).Updates(map[string]any{"enabled": true, "baseline_generation": 1, "status": models.MediaLibraryStatusListening}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.MediaLibraryStructureAutoState{}).Where("library_id=?", library.ID).Updates(map[string]any{"source_revision": 1, "diagnosed_revision": 1}).Error; err != nil {
		t.Fatal(err)
	}
	queue := NewQueueService(db, NewAuditService(db))
	now := time.Now().UTC()
	repair := models.MediaLibraryStructureRepair{ID: "cancel-original-repair", LibraryID: library.ID, OwnerID: actor.User.ID, Scope: "full", Phase: "queued", PlanJSON: "{}", StateJSON: "{}", CreatedAt: now, UpdatedAt: now}
	job, err := queue.EnqueueWith(EnqueueJobInput{System: true, JobType: JobTypeMediaLibraryRepair, DisplayName: "cancel original", ResourceKey: "library:" + uintID(library.ID), Payload: map[string]any{"repair_id": repair.ID}}, func(tx *gorm.DB, job models.Job) error {
		repair.JobID = &job.ID
		return RegisterCatalogPhysicalOwnerTx(tx, CatalogPhysicalWriteInput{LibraryID: library.ID, OwnerKind: CatalogPhysicalRepair, OwnerID: repair.ID}, func(tx *gorm.DB) error { return tx.Create(&repair).Error })
	})
	if err != nil {
		t.Fatal(err)
	}
	assertBlocked := func(want bool) {
		t.Helper()
		r, err := libraryReadiness(db, library.ID)
		if err != nil || r.Ready == want {
			t.Fatalf("readiness=%+v err=%v want blocked=%v", r, err, want)
		}
		owner, err := currentLibraryRepairOwner(db, library.ID)
		if err != nil || (owner != "") != want {
			t.Fatalf("owner=%q err=%v want blocked=%v", owner, err, want)
		}
		active, err := findActiveStructureRepair(db, library.ID, repair.Scope, repair.WorkKey)
		if want && (err != nil || active.ID != repair.ID) {
			t.Fatalf("active selector lost blocking repair: %+v %v", active, err)
		}
		if !want && !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("active selector returned cancelled repair: %+v %v", active, err)
		}
	}
	assertBlocked(true)
	if _, err := queue.Control(actor, job.ID, "cancel", RequestContext{}); err != nil {
		t.Fatal(err)
	}
	assertBlocked(false)
	var proof models.CatalogPhysicalWrite
	if err := db.First(&proof, "owner_kind=? AND owner_id=?", CatalogPhysicalRepair, repair.ID).Error; err != nil || proof.State != "admitted" {
		t.Fatalf("proof erased/settled: %+v %v", proof, err)
	}
	for _, phase := range []string{"executing", "reconciling"} {
		if err := db.Model(&repair).Update("phase", phase).Error; err != nil {
			t.Fatal(err)
		}
		assertBlocked(false)
	}
	// Exact no-I/O proof cannot excuse a different job, live/unacknowledged
	// lease, or physical/partial results. Restore each field after checking.
	for _, tc := range []struct {
		name            string
		model           any
		change, restore map[string]any
	}{
		{"wrong job", &proof, map[string]any{"job_id": "foreign-job"}, map[string]any{"job_id": job.ID}},
		{"missing proof", &proof, map[string]any{"state": "settled"}, map[string]any{"state": "admitted"}},
		{"entered", &proof, map[string]any{"state": "entered"}, map[string]any{"state": "admitted"}},
		{"quiescent", &proof, map[string]any{"state": "quiescent"}, map[string]any{"state": "admitted"}},
		{"partial", &repair, map[string]any{"succeeded_items": 1}, map[string]any{"succeeded_items": 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := db.Model(tc.model).Updates(tc.change).Error; err != nil {
				t.Fatal(err)
			}
			assertBlocked(true)
			if err := db.Model(tc.model).Updates(tc.restore).Error; err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, field := range []string{"lease_token_hash", "lease_expires_at"} {
		var value any = "unacknowledged"
		var cleared any = ""
		if field == "lease_expires_at" {
			value = now.Add(time.Minute)
			cleared = nil
		}
		if err := db.Model(&models.Job{}).Where("id=?", job.ID).Update(field, value).Error; err != nil {
			t.Fatal(err)
		}
		assertBlocked(true)
		if err := db.Model(&models.Job{}).Where("id=?", job.ID).Update(field, cleared).Error; err != nil {
			t.Fatal(err)
		}
	}
	assertBlocked(false)
	diagnosis, err := queue.Enqueue(EnqueueJobInput{System: true, JobType: JobTypeMediaLibraryStructureDiagnosis, DisplayName: "diagnose after cancel", ResourceKey: "structure-diagnosis-library:" + uintID(library.ID), Payload: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
	if err != nil || claim == nil || claim.Job.ID != diagnosis.ID {
		t.Fatalf("diagnosis still blocked: %+v %v", claim, err)
	}
	// User retry remains explicit; cancellation is not a permanent exemption.
	if err := db.Model(&models.Job{}).Where("id=?", job.ID).Update("status", models.JobStatusQueued).Error; err != nil {
		t.Fatal(err)
	}
	assertBlocked(true)
}
