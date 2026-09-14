package services

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestLibraryReadinessDiagnosisFindingsAndRepairLifecycle(t *testing.T) {
	s, db, actor, storage, profile := mediaLibraryTestService(t)
	l, err := s.Create(context.Background(), actor, testLibraryInput("readiness", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Model(&models.MediaLibrary{}).Where("id=?", l.ID).Updates(map[string]any{"enabled": true, "baseline_generation": 1, "status": models.MediaLibraryStatusListening, "structure_status": "issues", "structure_issue_count": 36}).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Model(&models.MediaLibraryStructureAutoState{}).Where("library_id=?", l.ID).Updates(map[string]any{"diagnosed_revision": 1, "source_revision": 1}).Error; err != nil {
		t.Fatal(err)
	}
	assertStatus := func(want string) {
		t.Helper()
		r, e := libraryReadiness(db, l.ID)
		if e != nil || r.ReadinessStatus != want {
			t.Fatalf("readiness=%+v err=%v want=%s", r, e, want)
		}
	}
	assertStatus("ready")
	jobID := "readiness-repair-job"
	if err = db.Create(&models.Job{ID: jobID, CreatedByKind: "system", JobType: JobTypeMediaLibraryRepair, Status: models.JobStatusQueued, DisplayName: "readiness repair", PayloadJSON: `{"repair_id":"readiness-repair"}`, CheckpointJSON: "{}", CreatedAt: time.Now(), UpdatedAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	repair := models.MediaLibraryStructureRepair{ID: "readiness-repair", OwnerID: actor.User.ID, LibraryID: l.ID, Scope: "full", Phase: "queued", PlanJSON: "{}", StateJSON: "{}", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	repair.JobID = &jobID
	if err = db.Create(&repair).Error; err != nil {
		t.Fatal(err)
	}
	assertStatus("repairing")
	job := models.Job{ID: jobID, JobType: JobTypeMediaArtifact, ResourceKey: mediaArtifactResourceKey(l.ID)}
	if blocked, e := libraryJobBlocked(db, job); e != nil || !blocked {
		t.Fatalf("artifact not blocked: %v %v", blocked, e)
	}
	job.JobType = JobTypeMediaLibraryRepair
	if blocked, e := libraryJobBlocked(db, job); e != nil || blocked {
		t.Fatalf("repair blocked: %v %v", blocked, e)
	}
	for _, phase := range []string{"executing", "failed"} {
		if err = db.Model(&repair).Update("phase", phase).Error; err != nil {
			t.Fatal(err)
		}
		if _, e := s.reconcile(context.Background(), l.ID, "full"); !errors.Is(e, errMediaLibraryEventReconcileDeferred) {
			t.Fatalf("phase %s scan not deferred: %v", phase, e)
		}
	}
	assertStatus("repair_failed")
	if err = db.Model(&repair).Update("phase", "completed").Error; err != nil {
		t.Fatal(err)
	}
	assertStatus("ready")
	var runs int64
	db.Model(&models.MediaLibraryScanRun{}).Where("library_id=?", l.ID).Count(&runs)
	if runs != 0 {
		t.Fatalf("deferred repair scans created %d runs", runs)
	}
}

func TestLibraryReadinessQueuedWorkWaitsWithoutAttemptOrUnrelatedStarvation(t *testing.T) {
	s, db, actor, storage, profile := mediaLibraryTestService(t)
	l, err := s.Create(context.Background(), actor, testLibraryInput("queue readiness", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	queue := NewQueueService(db, NewAuditService(db))
	for i := 0; i < 130; i++ {
		_, err = queue.Enqueue(EnqueueJobInput{System: true, JobType: "fake", DisplayName: fmt.Sprintf("waiting %d", i), ResourceKey: "library:" + uintID(l.ID), Payload: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
	}
	unrelated, err := queue.Enqueue(EnqueueJobInput{System: true, JobType: "fake", DisplayName: "unrelated", ResourceKey: "independent", Payload: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := queue.Claim([]string{"fake"})
	if err != nil || claim == nil || claim.Job.ID != unrelated.ID {
		t.Fatalf("unrelated starved claim=%+v err=%v", claim, err)
	}
	var changed int64
	db.Model(&models.Job{}).Where("resource_key=? AND (attempt_count<>0 OR status<>'queued')", "library:"+uintID(l.ID)).Count(&changed)
	if changed != 0 {
		t.Fatalf("waiting jobs were attempted: %d", changed)
	}
	// New service instance observes the same durable readiness, no memory flag.
	queue = NewQueueService(db, NewAuditService(db))
	claim, err = queue.Claim([]string{"fake"})
	if err != nil || claim != nil {
		t.Fatalf("restart claimed unready work: %+v %v", claim, err)
	}
}

func TestLibraryReadinessNewerCompletedRepairDoesNotMaskOlderUnsettledFailure(t *testing.T) {
	s, db, actor, storage, profile := mediaLibraryTestService(t)
	l, err := s.Create(context.Background(), actor, testLibraryInput("history readiness", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	db.Model(&models.MediaLibrary{}).Where("id=?", l.ID).Updates(map[string]any{"enabled": true, "baseline_generation": 1, "status": models.MediaLibraryStatusListening})
	db.Model(&models.MediaLibraryStructureAutoState{}).Where("library_id=?", l.ID).Update("diagnosed_revision", 1)
	now := time.Now().UTC()
	old := models.MediaLibraryStructureRepair{ID: "old-failure", OwnerID: actor.User.ID, LibraryID: l.ID, Scope: "full", Phase: "failed", PlanJSON: "{}", StateJSON: "{}", CreatedAt: now.Add(-time.Hour), UpdatedAt: now}
	if err = db.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	proof := models.CatalogPhysicalWrite{LibraryID: l.ID, OwnerKind: CatalogPhysicalRepair, OwnerID: old.ID, State: "admitted", Revision: 1, OwnerDigest: "test", EnteredAt: now, UpdatedAt: now}
	if err = db.Create(&proof).Error; err != nil {
		t.Fatal(err)
	}
	done := old
	done.ID = "verified-later"
	done.Phase = "completed"
	done.CreatedAt = now
	if err = db.Create(&done).Error; err != nil {
		t.Fatal(err)
	}
	r, err := libraryReadiness(db, l.ID)
	if err != nil || r.Ready || r.ReadinessStatus != "repair_failed" {
		t.Fatalf("newer repair masked older admitted failure: %+v %v", r, err)
	}
	if err = db.Model(&proof).Update("state", "quiescent").Error; err != nil {
		t.Fatal(err)
	}
	r, err = libraryReadiness(db, l.ID)
	if err != nil || r.Ready {
		t.Fatalf("unknown prior write ignored: %+v %v", r, err)
	}
}

func TestLibraryReadinessQueuedRetryBlocksEvenAfterPriorPhysicalSettlement(t *testing.T) {
	s, db, actor, storage, profile := mediaLibraryTestService(t)
	l, err := s.Create(context.Background(), actor, testLibraryInput("queued retry readiness", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Model(&models.MediaLibrary{}).Where("id=?", l.ID).Updates(map[string]any{"enabled": true, "baseline_generation": 1, "status": models.MediaLibraryStatusListening}).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Model(&models.MediaLibraryStructureAutoState{}).Where("library_id=?", l.ID).Updates(map[string]any{"diagnosed_revision": 1, "source_revision": 1}).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	repair := models.MediaLibraryStructureRepair{ID: "queued-retry", OwnerID: actor.User.ID, LibraryID: l.ID, Scope: "full", Phase: "queued", PlanJSON: "{}", StateJSON: "{}", CreatedAt: now, UpdatedAt: now}
	if err = db.Create(&repair).Error; err != nil {
		t.Fatal(err)
	}
	proof := models.CatalogPhysicalWrite{LibraryID: l.ID, OwnerKind: CatalogPhysicalRepair, OwnerID: repair.ID, State: "settled", Revision: 2, OwnerDigest: "test", EnteredAt: now, SettledAt: &now, UpdatedAt: now}
	if err = db.Create(&proof).Error; err != nil {
		t.Fatal(err)
	}
	r, err := libraryReadiness(db, l.ID)
	if err != nil || r.Ready || r.ReadinessStatus != "repairing" {
		t.Fatalf("queued retry did not hold readiness: %+v %v", r, err)
	}
	if err = db.Model(&repair).Updates(map[string]any{"phase": "failed", "succeeded_items": 1, "failed_items": 1}).Error; err != nil {
		t.Fatal(err)
	}
	r, err = libraryReadiness(db, l.ID)
	if err != nil || !r.Ready {
		t.Fatalf("settled partial result still blocked fresh work: %+v %v", r, err)
	}
}

func TestLibraryReadinessCancelledAdmittedRepairRequiresReleasedLease(t *testing.T) {
	s, db, actor, storage, profile := mediaLibraryTestService(t)
	l, err := s.Create(context.Background(), actor, testLibraryInput("cancelled admitted readiness", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Model(&models.MediaLibrary{}).Where("id=?", l.ID).Updates(map[string]any{"enabled": true, "baseline_generation": 1, "status": models.MediaLibraryStatusListening}).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Model(&models.MediaLibraryStructureAutoState{}).Where("library_id=?", l.ID).Updates(map[string]any{"diagnosed_revision": 1, "source_revision": 1}).Error; err != nil {
		t.Fatal(err)
	}
	now, expires := time.Now().UTC(), time.Now().UTC().Add(time.Minute)
	job := models.Job{ID: "cancelled-admitted-job", CreatedByKind: "system", JobType: JobTypeMediaLibraryRepair, Status: models.JobStatusCancelled, DisplayName: "cancelled admitted", PayloadJSON: `{"repair_id":"cancelled-admitted"}`, CheckpointJSON: "{}", LeaseTokenHash: "not-released", LeaseExpiresAt: &expires, CreatedAt: now, UpdatedAt: now}
	if err = db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	repair := models.MediaLibraryStructureRepair{ID: "cancelled-admitted", OwnerID: actor.User.ID, LibraryID: l.ID, JobID: &job.ID, Scope: "full", Phase: "failed", PlanJSON: "{}", StateJSON: "{}", CreatedAt: now, UpdatedAt: now}
	if err = db.Create(&repair).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&models.CatalogPhysicalWrite{LibraryID: l.ID, OwnerKind: CatalogPhysicalRepair, OwnerID: repair.ID, State: "admitted", Revision: 1, JobID: job.ID, OwnerDigest: "test", EnteredAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if r, err := libraryReadiness(db, l.ID); err != nil || r.ReadinessStatus != "repair_failed" {
		t.Fatalf("active lease was treated as no-I/O cancellation: %+v %v", r, err)
	}
	if err = db.Model(&job).Updates(map[string]any{"lease_token_hash": "", "lease_expires_at": nil}).Error; err != nil {
		t.Fatal(err)
	}
	if r, err := libraryReadiness(db, l.ID); err != nil || !r.Ready {
		t.Fatalf("released cancelled admission remained blocking: %+v %v", r, err)
	}
}

func TestLibraryReadinessCredentialRecoveryAndRiskAreDistinct(t *testing.T) {
	s, db, actor, storage, profile := mediaLibraryTestService(t)
	l, err := s.Create(context.Background(), actor, testLibraryInput("credential readiness", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	c := models.Connection{Name: "readiness account", NameNormalized: "readiness account", Provider: models.ConnectionProviderPan115, CredentialCiphertext: "encrypted", Enabled: true, Revision: 1, LastHealthStatus: "offline", LastHealthErrorCode: "pan115_auth_expired", CreatedAt: now, UpdatedAt: now}
	if err = db.Create(&c).Error; err != nil {
		t.Fatal(err)
	}
	db.Model(&models.Storage{}).Where("id=?", storage.ID).Update("connection_id", c.ID)
	db.Model(&models.MediaLibrary{}).Where("id=?", l.ID).Updates(map[string]any{"enabled": true, "baseline_generation": 1, "status": models.MediaLibraryStatusListening})
	db.Model(&models.MediaLibraryStructureAutoState{}).Where("library_id=?", l.ID).Update("diagnosed_revision", 1)
	job := models.Job{ID: "credential-repair-job", CreatedByKind: "system", JobType: JobTypeMediaLibraryRepair, Status: models.JobStatusQueued, DisplayName: "credential repair", ResourceKey: "library:" + uintID(l.ID), PayloadJSON: `{"repair_id":"credential-repair"}`, CheckpointJSON: "{}", CreatedAt: now, UpdatedAt: now}
	if err = db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	repair := models.MediaLibraryStructureRepair{ID: "credential-repair", OwnerID: actor.User.ID, LibraryID: l.ID, JobID: &job.ID, Scope: "full", Phase: "queued", PlanJSON: "{}", StateJSON: "{}", CreatedAt: now, UpdatedAt: now}
	if err = db.Create(&repair).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&models.CatalogPhysicalWrite{LibraryID: l.ID, OwnerKind: CatalogPhysicalRepair, OwnerID: repair.ID, Revision: 1, State: "admitted", JobID: job.ID, OwnerDigest: "test", EnteredAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if blocked, e := libraryJobBlocked(db, job); e != nil || !blocked {
		t.Fatalf("expired repair not held: %v %v", blocked, e)
	}
	db.Model(&c).Updates(map[string]any{"last_health_status": "online", "last_health_error_code": "", "revision": 2})
	if blocked, e := libraryJobBlocked(db, job); e != nil || blocked {
		t.Fatalf("renewed repair not released: %v %v", blocked, e)
	}
	db.Model(&c).Updates(map[string]any{"last_health_status": "offline", "last_health_error_code": "pan115_rate_limited"})
	r, e := libraryReadiness(db, l.ID)
	if e != nil || r.ReadinessStatus == "credentials_required" || r.ReadinessStatus != "repairing" {
		t.Fatalf("temporary risk latched auth or cleared repair: %+v %v", r, e)
	}
	db.Model(&c).Update("enabled", false)
	r, e = libraryReadiness(db, l.ID)
	if e != nil || r.Ready || r.ReadinessStatus != "unavailable" {
		t.Fatalf("disabled connection ready: %+v %v", r, e)
	}
}
