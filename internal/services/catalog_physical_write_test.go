package services

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func catalogPhysicalRepairFixture(t *testing.T, queued bool) (*gorm.DB, models.MediaLibraryStructureRepair, CatalogPhysicalWriteInput) {
	t.Helper()
	s, db, actor, storage, profile := mediaLibraryTestService(t)
	library, err := s.Create(context.Background(), actor, testLibraryInput("physical fence", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	row := models.MediaLibraryStructureRepair{ID: "physical-repair", OwnerID: actor.User.ID, LibraryID: library.ID, Scope: models.MediaLibraryStructureScopeWork, PlanJSON: "{}", StateJSON: "{}", Phase: "executing", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	input := CatalogPhysicalWriteInput{LibraryID: library.ID, OwnerKind: CatalogPhysicalRepair, OwnerID: row.ID, ActorID: actor.User.ID}
	if queued {
		expires := time.Now().UTC().Add(time.Minute)
		job := models.Job{ID: "physical-repair-job", CreatedByKind: "system", JobType: JobTypeMediaLibraryRepair, Status: models.JobStatusRunning, LeaseTokenHash: leaseHash("physical-lease"), LeaseExpiresAt: &expires, Generation: 1, PayloadJSON: fmt.Sprintf(`{"repair_id":%q}`, row.ID)}
		if err := db.Create(&job).Error; err != nil {
			t.Fatal(err)
		}
		row.JobID = &job.ID
		input.Job = &ClaimedJob{Job: job, LeaseToken: "physical-lease"}
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	return db, row, input
}

func TestCatalogPhysicalEvidenceRequiresTransactionRealOwnerAndLiveLease(t *testing.T) {
	db, repair, input := catalogPhysicalRepairFixture(t, true)
	if _, err := EnterCatalogPhysicalWriteTx(db, input); err == nil {
		t.Fatal("nontransaction admission succeeded")
	}
	for _, mutation := range []func(*CatalogPhysicalWriteInput){
		func(i *CatalogPhysicalWriteInput) { i.OwnerID = "missing" },
		func(i *CatalogPhysicalWriteInput) { i.LibraryID++ },
		func(i *CatalogPhysicalWriteInput) { i.Job = &ClaimedJob{Job: i.Job.Job, LeaseToken: "wrong"} },
	} {
		bad := input
		mutation(&bad)
		if err := db.Transaction(func(tx *gorm.DB) error { _, err := EnterCatalogPhysicalWriteTx(tx, bad); return err }); err == nil {
			t.Fatal("unbound owner acquired permit")
		}
	}
	var permit CatalogPhysicalWritePermit
	if err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		permit, err = EnterCatalogPhysicalWriteTx(tx, input)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return SettleCatalogPhysicalWriteTx(tx, permit, input.Job) }); err == nil {
		t.Fatal("unfinished domain settled")
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&repair).Updates(map[string]any{"phase": "completed", "finished_at": time.Now().UTC()}).Error; err != nil {
			return err
		}
		return SettleCatalogPhysicalWriteTx(tx, permit, input.Job)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return AssertCatalogPhysicalDrainedTx(tx, input.LibraryID) }); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogPhysicalLostLeaseCannotResumeUntilOriginalExitAck(t *testing.T) {
	db, repair, input := catalogPhysicalRepairFixture(t, true)
	var first CatalogPhysicalWritePermit
	if err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		first, err = EnterCatalogPhysicalWriteTx(tx, input)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.Job{}).Where("id=?", input.Job.Job.ID).Update("lease_token_hash", leaseHash("successor")).Error; err != nil {
		t.Fatal(err)
	}
	input.Job = &ClaimedJob{Job: input.Job.Job, LeaseToken: "successor"}
	if err := db.Transaction(func(tx *gorm.DB) error { _, err := EnterCatalogPhysicalWriteTx(tx, input); return err }); err == nil {
		t.Fatal("lease revocation falsely proved old I/O exited")
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return QuiesceCatalogPhysicalWriteTx(tx, first) }); err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return AssertCatalogPhysicalDrainedTx(tx, input.LibraryID) }); err == nil {
		t.Fatal("quiescence erased unresolved work")
	}
	var second CatalogPhysicalWritePermit
	if err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		second, err = EnterCatalogPhysicalWriteTx(tx, input)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if second.evidence.Revision != first.evidence.Revision+1 {
		t.Fatal("resume did not advance exact permit revision")
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return QuiesceCatalogPhysicalWriteTx(tx, first) }); err == nil {
		t.Fatal("old callback quiesced successor execution")
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&repair).Updates(map[string]any{"phase": "completed", "finished_at": time.Now().UTC()}).Error; err != nil {
			return err
		}
		return SettleCatalogPhysicalWriteTx(tx, second, input.Job)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogPhysicalQuiescentRecoveryPreservesFrozenSourceAndOwner(t *testing.T) {
	db, repair, input := catalogPhysicalRepairFixture(t, false)
	var permit CatalogPhysicalWritePermit
	if err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		permit, err = EnterCatalogPhysicalWriteTx(tx, input)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return QuiesceCatalogPhysicalWriteTx(tx, permit) }); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&repair).Update("plan_json", `{"different":true}`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { _, err := EnterCatalogPhysicalWriteTx(tx, input); return err }); err == nil {
		t.Fatal("changed recovery plan accepted")
	}
	if err := db.Model(&repair).Updates(map[string]any{"plan_json": "{}", "phase": "failed"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.MediaLibrary{}).Where("id=?", input.LibraryID).Update("relative_root", "new-source").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { _, err := EnterCatalogPhysicalWriteTx(tx, input); return err }); err == nil {
		t.Fatal("changed source accepted")
	}
}

func TestCatalogPhysicalRetirementAndEnterAreTransactionallyExclusive(t *testing.T) {
	for _, retirementFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(retirementFirst), func(t *testing.T) {
			db, repair, input := catalogPhysicalRepairFixture(t, false)
			// A new synchronous durable repair is created in the admission tx.
			if err := db.Delete(&repair).Error; err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{})
			release := make(chan struct{})
			var wg sync.WaitGroup
			var firstErr, secondErr error
			retire := func(tx *gorm.DB) error {
				if err := AssertCatalogPhysicalDrainedTx(tx, input.LibraryID); err != nil {
					return err
				}
				return tx.Create(&models.MediaLibraryRetirement{ID: "retire", LibraryID: input.LibraryID, ActorID: input.ActorID, JobID: "retire-job", Phase: "queued", SourceEpoch: 1, Revision: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}).Error
			}
			enter := func(tx *gorm.DB) error {
				if err := tx.Create(&repair).Error; err != nil {
					return err
				}
				_, err := EnterCatalogPhysicalWriteTx(tx, input)
				return err
			}
			first, second := enter, retire
			if retirementFirst {
				first, second = retire, enter
			}
			wg.Add(2)
			go func() {
				defer wg.Done()
				firstErr = db.Transaction(func(tx *gorm.DB) error {
					if err := first(tx); err != nil {
						return err
					}
					close(entered)
					<-release
					return nil
				})
			}()
			<-entered
			go func() { defer wg.Done(); secondErr = db.Transaction(second) }()
			close(release)
			wg.Wait()
			if firstErr != nil || secondErr == nil {
				t.Fatalf("first=%v second=%v", firstErr, secondErr)
			}
		})
	}
}

func TestCatalogPhysicalLegacyFailedOwnerBlocksWithoutDisablingOrFenceMutation(t *testing.T) {
	db, repair, input := catalogPhysicalRepairFixture(t, true)
	if err := db.Model(&repair).Update("phase", "failed").Error; err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-365 * 24 * time.Hour)
	if err := db.Model(&models.Job{}).Where("id=?", input.Job.Job.ID).Updates(map[string]any{"status": "cancelled", "lease_expires_at": past}).Error; err != nil {
		t.Fatal(err)
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := AssertCatalogPhysicalDrainedTx(tx, input.LibraryID); err != nil {
			return err
		}
		return tx.Model(&models.MediaLibrary{}).Where("id=?", input.LibraryID).Update("relative_root", "changed").Error
	})
	if err == nil {
		t.Fatal("old cancelled/failed physical owner ignored")
	}
	var library models.MediaLibrary
	if err := db.First(&library, input.LibraryID).Error; err != nil {
		t.Fatal(err)
	}
	if library.RelativeRoot != "/" {
		t.Fatal("refused recovery mutated source")
	}
	if !errors.As(err, new(*AppError)) {
		t.Fatalf("expected safe conflict, got %v", err)
	}
}

func TestCatalogPhysicalOnlyNewRegisteredOwnersProveNotEntered(t *testing.T) {
	db, repair, input := catalogPhysicalRepairFixture(t, false)
	if err := db.Transaction(func(tx *gorm.DB) error {
		return RegisterCatalogPhysicalOwnerTx(tx, input, func(*gorm.DB) error { return nil })
	}); err == nil {
		t.Fatal("admitted proof retrofitted onto old owner")
	}
	if err := db.Delete(&repair).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return RegisterCatalogPhysicalOwnerTx(tx, input, func(tx *gorm.DB) error { return tx.Create(&repair).Error })
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := AssertCatalogPhysicalDrainedTx(tx, input.LibraryID); err != nil {
			return err
		}
		return tx.Create(&models.MediaLibraryRetirement{ID: "registered-retire", LibraryID: input.LibraryID, ActorID: input.ActorID, JobID: "retire-job", Phase: "queued", Revision: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}).Error
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { _, err := EnterCatalogPhysicalWriteTx(tx, input); return err }); err == nil {
		t.Fatal("queued owner entered after retirement acquired exclusive gate")
	}
	var row models.CatalogPhysicalWrite
	if err := db.Where("owner_id=?", repair.ID).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.State != "admitted" {
		t.Fatalf("failed entry changed proof=%s", row.State)
	}
}

func TestCatalogPhysicalRegisteredEntryBecomesDrainBlocking(t *testing.T) {
	db, repair, input := catalogPhysicalRepairFixture(t, false)
	if err := db.Delete(&repair).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return RegisterCatalogPhysicalOwnerTx(tx, input, func(tx *gorm.DB) error { return tx.Create(&repair).Error })
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { _, err := EnterCatalogPhysicalWriteTx(tx, input); return err }); err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return AssertCatalogPhysicalDrainedTx(tx, input.LibraryID) }); err == nil {
		t.Fatal("admitted proof remained ignorable after entry")
	}
}

func TestCatalogPhysicalRepairAdmissionAndEntryStayWithOldestExactOwner(t *testing.T) {
	db, first, input := catalogPhysicalRepairFixture(t, false)
	if err := db.Delete(&first).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return RegisterCatalogPhysicalOwnerTx(tx, input, func(tx *gorm.DB) error { return tx.Create(&first).Error })
	}); err != nil {
		t.Fatal(err)
	}

	second := first
	second.ID = "competing-repair"
	second.CreatedAt = first.CreatedAt.Add(time.Second)
	second.UpdatedAt = second.CreatedAt
	secondInput := input
	secondInput.OwnerID = second.ID
	if err := db.Transaction(func(tx *gorm.DB) error {
		return RegisterCatalogPhysicalOwnerTx(tx, secondInput, func(tx *gorm.DB) error { return tx.Create(&second).Error })
	}); err == nil {
		t.Fatal("competing repair was admitted while the original repair owned readiness")
	}
	var secondRows int64
	if err := db.Model(&models.MediaLibraryStructureRepair{}).Where("id=?", second.ID).Count(&secondRows).Error; err != nil || secondRows != 0 {
		t.Fatalf("rejected repair was persisted: count=%d err=%v", secondRows, err)
	}

	var firstPermit CatalogPhysicalWritePermit
	if err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		firstPermit, err = EnterCatalogPhysicalWriteTx(tx, input)
		return err
	}); err != nil {
		t.Fatalf("exact admitted repair did not enter: %v", err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return QuiesceCatalogPhysicalWriteTx(tx, firstPermit) }); err != nil {
		t.Fatal(err)
	}

	// Simulate a legacy/raced second admitted row. The oldest unresolved owner
	// remains authoritative; the later row cannot use the first repair's block
	// as a generic exemption.
	if err := db.Create(&second).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.Create(&models.CatalogPhysicalWrite{LibraryID: input.LibraryID, OwnerKind: CatalogPhysicalRepair, OwnerID: second.ID, Revision: 1, State: "admitted", OwnerDigest: "fixture", EnteredAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		_, err := EnterCatalogPhysicalWriteTx(tx, secondInput)
		return err
	}); err == nil {
		t.Fatal("later repair crossed the original exact-owner recovery gate")
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		_, err := EnterCatalogPhysicalWriteTx(tx, input)
		return err
	}); err != nil {
		t.Fatalf("oldest exact quiescent owner could not recover: %v", err)
	}
}

func TestCatalogPhysicalHistoricalArtifactCompletionAndAmbiguity(t *testing.T) {
	db, repair, input := catalogPhysicalRepairFixture(t, false)
	if err := db.Delete(&repair).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	job := models.Job{ID: "legacy-artifact-job", CreatedByKind: "system", JobType: JobTypeMediaArtifact, Status: models.JobStatusCompleted, FinishedAt: &now}
	if err := db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	run := models.MediaArtifactRun{ID: "legacy-artifact-run", LibraryID: input.LibraryID, JobID: &job.ID, Generation: 1, Status: models.MediaArtifactStatusCompleted, CleanupStatus: models.MediaArtifactCleanupPending, FinishedAt: &now, ExpectedCount: 1, WrittenCount: 1, PolicyJSON: fmt.Sprintf(`{"library_id":%d,"generation":1,"scan_run_id":0,"target_kind":"local_projection"}`, input.LibraryID)}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	drain := func() error {
		return db.Transaction(func(tx *gorm.DB) error { return AssertCatalogPhysicalDrainedTx(tx, input.LibraryID) })
	}
	if err := drain(); err != nil {
		t.Fatalf("completed legacy Job and cleanup-ineligible policy rejected: %v", err)
	}
	if err := db.Model(&run).Update("policy_json", fmt.Sprintf(`{"library_id":%d,"generation":1,"scan_run_id":1,"cleanup_eligible":true,"target_kind":"local_projection"}`, input.LibraryID)).Error; err != nil {
		t.Fatal(err)
	}
	if err := drain(); err == nil {
		t.Fatal("pending eligible cleanup guessed complete")
	}
	if err := db.Model(&run).Update("policy_json", "invalid old policy").Error; err != nil {
		t.Fatal(err)
	}
	if err := drain(); err == nil || ErrorCode(err) != CodeConflict {
		t.Fatalf("malformed policy should yield safe unresolved conflict: %v", err)
	}
	if err := db.Model(&run).Updates(map[string]any{"cleanup_status": "completed", "cleanup_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := drain(); err != nil {
		t.Fatal(err)
	}
	past := now.Add(-time.Hour)
	if err := db.Model(&job).Updates(map[string]any{"status": "running", "lease_token_hash": "old-unacknowledged", "lease_expires_at": past}).Error; err != nil {
		t.Fatal(err)
	}
	if err := drain(); err == nil {
		t.Fatal("legacy completion ignored an unacknowledged physical lease")
	}
}
