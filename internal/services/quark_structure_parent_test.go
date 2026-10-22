package services

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
)

func (f quarkStructureFixture) parent(t *testing.T, allowBlocked ...bool) (models.TransferTask, ClaimedJob) {
	t.Helper()
	if f.s.queue == nil {
		f.s.queue = NewQueueService(f.s.db, f.s.audit)
	}
	id := uuid.NewString()
	downloadJob, err := f.s.queue.Enqueue(EnqueueJobInput{OwnerID: f.owner, JobType: "download", DisplayName: "isolated download", Payload: map[string]string{"download_task_id": id}})
	if err != nil {
		t.Fatal(err)
	}
	download := models.DownloadTask{ID: id, JobID: downloadJob.ID, OwnerID: f.owner, Phase: models.DownloadTaskStatusCompleted, ProfileID: f.library.ProfileID}
	if err := f.s.db.Create(&download).Error; err != nil {
		t.Fatal(err)
	}
	job, err := f.s.queue.Enqueue(EnqueueJobInput{OwnerID: f.owner, JobType: "transfer", DisplayName: "isolated parent", Payload: transferJobPayload{TransferTaskID: id}})
	if err != nil {
		t.Fatal(err)
	}
	parent := models.TransferTask{ID: id, JobID: job.ID, OwnerID: f.owner, LibraryID: f.library.ID, DownloadTaskID: download.ID, Phase: models.TransferTaskStatusPlanning}
	if err := f.s.db.Create(&parent).Error; err != nil {
		t.Fatal(err)
	}
	claim, err := f.s.queue.Claim([]string{"transfer"})
	if err == nil && claim == nil && len(allowBlocked) > 0 && allowBlocked[0] {
		var stored models.Job
		if err := f.s.db.First(&stored, "id = ?", job.ID).Error; err != nil {
			t.Fatal(err)
		}
		return parent, ClaimedJob{Job: stored}
	}
	if err != nil || claim == nil {
		t.Fatalf("parent claim: %v", err)
	}
	return parent, *claim
}

func TestQuarkStructureTransferWorkerPreservesWait(t *testing.T) {
	f := newQuarkStructureFixture(t)
	parent, claim := f.parent(t)
	var storage models.Storage
	f.s.db.First(&storage, f.library.StorageID)
	async := &quarkStructureReceiptFake{quarkTransferFake: f.driver, kind: "rename", code: quark.CodePending, receipts: map[string]cloud.Item{}}
	f.s.connections.drivers[*storage.ConnectionID] = async
	identity := models.DataSourceIdentity{Kind: models.DataSourceKindProvider, ProviderType: models.StorageTypeQuark, ConnectionIdentity: uintID(*storage.ConnectionID), StorageScope: uintID(storage.ID)}
	route, _ := json.Marshal(identity)
	tmdb := int64(42)
	metadata, _ := json.Marshal(MediaIdentitySnapshot{Version: 1, Revision: 1, Source: mediaIdentitySourceAutomatic, Status: mediaIdentityStatusVerified, TMDBID: &tmdb, MediaType: "tv", Title: "Show", Category: "TV"})
	manifest := downloadpkg.Manifest{Complete: true, Files: []downloadpkg.File{{RelativePath: "Show.S02E03.mkv", Size: minimumAutomaticTransferVideoBytes + 1}}}
	manifestJSON, _ := json.Marshal(manifest)
	if err := f.s.db.Model(&models.DownloadTask{}).Where("id = ?", parent.DownloadTaskID).Updates(map[string]any{"scrape_status": "completed_verified", "scrape_tmdb_id": tmdb, "scrape_title": "Show", "scrape_media_type": "tv", "scrape_category": "TV", "identity_revision": 1, "identity_source": mediaIdentitySourceAutomatic, "identity_status": mediaIdentityStatusVerified, "identity_snapshot_json": string(metadata), "provider_type": models.DownloaderTypeQuarkShare, "target_storage_type": models.StorageTypeQuark, "source_data_source_json": string(route), "target_data_source_json": string(route), "transfer_route_kind": models.TransferRouteSameSourceProvider, "transfer_route_version": models.TransferRouteVersionCurrent}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.Model(&parent).Update("manifest_json", string(manifestJSON)).Error; err != nil {
		t.Fatal(err)
	}
	transfer := NewTransferService(f.s.db, f.s.audit, f.s.queue, zerolog.Nop())
	transfer.SetMediaLibraryStructureService(f.s)
	result := NewTransferWorker(transfer).Run(context.Background(), nil, claim)
	if result.ErrorCode != quarkStructurePending || result.RetryAt == nil || retryWaitConsumesFailureBudget(result.ErrorCode) {
		t.Fatalf("actual transfer flattened pending repair: %+v", result)
	}
	var saved models.TransferTask
	f.s.db.First(&saved, "id = ?", parent.ID)
	if saved.Phase != models.TransferTaskStatusPlanning || saved.LastErrorCode != "" {
		t.Fatal("pending structure marked parent failed")
	}
	_, state := f.stored(t)
	if state.ParentTransferID != parent.ID || state.ObservationHoldVersion != 1 {
		t.Fatal("worker did not bind original parent")
	}
}

func TestQuarkStructureOriginalTransferOwnsRepairAndWait(t *testing.T) {
	f := newQuarkStructureFixture(t)
	parent, claim := f.parent(t)
	var storage models.Storage
	f.s.db.First(&storage, f.library.StorageID)
	async := &quarkStructureReceiptFake{quarkTransferFake: f.driver, kind: "rename", code: quark.CodePending, receipts: map[string]cloud.Item{}}
	f.s.connections.drivers[*storage.ConnectionID] = async
	before := f.head(t)
	for range 2 {
		err := f.s.EnsureWorkLayout(context.Background(), f.owner, f.library.ID, 42, "tv", &claim)
		var wait *structureRepairWaitError
		if !errors.As(err, &wait) || wait.result.ErrorCode != quarkStructurePending {
			t.Fatalf("parent wait lost: %v", err)
		}
	}
	repair, state := f.stored(t)
	if repair.JobID == nil || *repair.JobID != parent.JobID || state.ParentTransferID != parent.ID || state.ObservationHoldVersion != 1 || async.accepts != 1 {
		t.Fatal("parent/operation binding lost")
	}
	writes := [3]int{f.driver.nextID, f.driver.moveCalls, f.driver.renameCalls}
	if err := f.run(); err == nil {
		t.Fatal("unclaimed restart adopted a parent repair")
	}
	bad := claim
	bad.LeaseToken = "expired"
	if err := f.s.EnsureWorkLayout(context.Background(), f.owner, f.library.ID, 42, "tv", &bad); err == nil {
		t.Fatal("bad parent lease resumed repair")
	}
	if writes != [3]int{f.driver.nextID, f.driver.moveCalls, f.driver.renameCalls} {
		t.Fatal("bad owner performed writes")
	}
	async.settle = true
	if err := f.s.EnsureWorkLayout(context.Background(), f.owner, f.library.ID, 42, "tv", &claim); err != nil {
		t.Fatal(err)
	}
	f.assertCompleted(t, before)
}

func TestQuarkStructureCancelledParentCannotBeAdopted(t *testing.T) {
	f := newQuarkStructureFixture(t)
	_, claim := f.parent(t)
	var storage models.Storage
	f.s.db.First(&storage, f.library.StorageID)
	async := &quarkStructureReceiptFake{quarkTransferFake: f.driver, kind: "rename", code: cloud.CodeMutationUnknown, receipts: map[string]cloud.Item{}}
	f.s.connections.drivers[*storage.ConnectionID] = async
	if err := f.s.EnsureWorkLayout(context.Background(), f.owner, f.library.ID, 42, "tv", &claim); err == nil {
		t.Fatal("unknown was accepted")
	}
	actor := Actor{User: models.User{ID: f.owner}, Permissions: map[string]struct{}{authz.PermissionJobsControlAll: {}, authz.PermissionJobsReadAll: {}}}
	if _, err := f.s.queue.Control(actor, claim.Job.ID, "cancel", RequestContext{}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.queue.AcknowledgeInterrupt(claim.Job.ID, claim.LeaseToken); err != nil {
		t.Fatal(err)
	}
	_, next := f.parent(t, true)
	if next.LeaseToken != "" {
		t.Fatal("unresolved physical owner allowed a new transfer claim")
	}
	writes := [3]int{f.driver.nextID, f.driver.moveCalls, f.driver.renameCalls}
	if err := f.s.EnsureWorkLayout(context.Background(), f.owner, f.library.ID, 42, "tv", &next); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("new transfer adopted cancelled repair: %v", err)
	}
	_, state := f.stored(t)
	if state.ObservationHoldVersion != 1 || state.QuarkMutation == nil || writes != [3]int{f.driver.nextID, f.driver.moveCalls, f.driver.renameCalls} {
		t.Fatal("cancelled owner lost protection")
	}
}

type quarkStructurePreflightConflict struct {
	*quarkTransferFake
	exact  bool
	target string
}

func (f *quarkStructurePreflightConflict) Move(ctx context.Context, id, parent string) error {
	if id == "agent:"+f.target {
		err := cloud.Error(cloud.CodeConflict, false, nil)
		if f.exact {
			return &quark.OperationNotSubmittedError{Err: err}
		}
		return err
	}
	return f.quarkTransferFake.Move(ctx, id, parent)
}
func TestQuarkStructureOnlyPositiveNoDispatchClearsCursor(t *testing.T) {
	for _, exact := range []bool{true, false} {
		name := "generic_conflict_keeps_hold"
		if exact {
			name = "known_preflight_conflict_preserves_partial_success"
		}
		t.Run(name, func(t *testing.T) {
			f := newQuarkStructureFixture(t)
			var storage models.Storage
			f.s.db.First(&storage, f.library.StorageID)
			f.s.connections.drivers[*storage.ConnectionID] = &quarkStructurePreflightConflict{quarkTransferFake: f.driver, exact: exact, target: f.plan.Items[0].ProviderID}
			before := f.head(t)
			if err := f.run(); err == nil {
				t.Fatal("conflict disappeared")
			}
			repair, state := f.stored(t)
			if exact {
				if state.QuarkMutation != nil || state.ObservationHoldVersion != 0 || state.Stage != "partial_finalized" || repair.SucceededItems != len(f.plan.Items)-1 || f.head(t) != before+1 {
					t.Fatalf("known unsubmitted conflict stranded plan: %s hold=%d successful=%d", state.Stage, state.ObservationHoldVersion, repair.SucceededItems)
				}
			} else if state.QuarkMutation == nil || state.ObservationHoldVersion != 1 || f.head(t) != before {
				t.Fatal("generic conflict was interpreted as no dispatch")
			}
		})
	}
}
