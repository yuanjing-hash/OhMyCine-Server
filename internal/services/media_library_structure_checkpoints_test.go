package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
)

type batchCheckpointStructureBackend struct{ batches []int }

func (*batchCheckpointStructureBackend) StorageType() string                { return models.StorageTypePan115 }
func (*batchCheckpointStructureBackend) SupportsStructureRepairBatch() bool { return true }
func (*batchCheckpointStructureBackend) ValidateRecycle(context.Context, StructureBoundary) error {
	return nil
}
func (b *batchCheckpointStructureBackend) Recycle(_ context.Context, _ StructureBoundary, items []StructureRecycleItem, _ StructureProgress) error {
	b.batches = append(b.batches, len(items))
	return nil
}
func (b *batchCheckpointStructureBackend) Apply(_ context.Context, _ StructureBoundary, items []StructurePlanItem, _ StructureProgress) error {
	b.batches = append(b.batches, len(items))
	return nil
}

type checkpointStructureBackend struct {
	fail  map[string]bool
	calls []string
}

func (*checkpointStructureBackend) StorageType() string { return models.StorageTypeLocal }
func (*checkpointStructureBackend) ValidateRecycle(context.Context, StructureBoundary) error {
	return nil
}
func (b *checkpointStructureBackend) Recycle(_ context.Context, _ StructureBoundary, items []StructureRecycleItem, _ StructureProgress) error {
	for _, item := range items {
		b.calls = append(b.calls, item.SourceRelative)
		if b.fail[item.SourceRelative] {
			return errors.New("item failure")
		}
	}
	return nil
}
func (b *checkpointStructureBackend) Apply(_ context.Context, _ StructureBoundary, items []StructurePlanItem, _ StructureProgress) error {
	for _, item := range items {
		b.calls = append(b.calls, item.SourceRelative)
		if b.fail[item.SourceRelative] {
			return errors.New("item failure")
		}
	}
	return nil
}

func TestStructureRepairCheckpointsContinueBlockAndRetryOnlyRemaining(t *testing.T) {
	s, repair, _, _, _ := catalogStructureRepairFixture(t)
	plan := StructurePlan{Version: 1, LibraryID: repair.LibraryID, Items: []StructurePlanItem{
		{Kind: "video", SourceRelative: "a.mkv", TargetRelative: "x.mkv"},
		{Kind: "video", SourceRelative: "b.mkv", TargetRelative: "y.mkv"},
		{Kind: "video", SourceRelative: "c.mkv", TargetRelative: "z.mkv"},
		{Kind: "video", SourceRelative: "d.mkv", TargetRelative: "b.mkv"},
	}}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	repair.PlanJSON, repair.TotalItems = string(raw), len(plan.Items)
	if err := s.db.Model(&models.MediaLibraryStructureRepair{}).Where("id = ?", repair.ID).Updates(map[string]any{"plan_json": repair.PlanJSON, "total_items": repair.TotalItems}).Error; err != nil {
		t.Fatal(err)
	}
	firstBackend := &checkpointStructureBackend{fail: map[string]bool{"b.mkv": true}}
	first := s.executeStructureRepairItems(context.Background(), fastScanTestRuntime{}, repair, plan, StructureBoundary{}, firstBackend, nil)
	if first.GlobalErr != nil || first.Succeeded != 2 || first.Failed != 1 || first.Blocked != 1 {
		t.Fatalf("first=%+v", first)
	}
	if !reflect.DeepEqual(firstBackend.calls, []string{"a.mkv", "b.mkv", "c.mkv"}) {
		t.Fatalf("first calls=%v", firstBackend.calls)
	}
	var persisted models.MediaLibraryStructureRepair
	if err := s.db.First(&persisted, "id = ?", repair.ID).Error; err != nil || persisted.SucceededItems != 2 || persisted.FailedItems != 1 || persisted.BlockedItems != 1 {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}
	actor := Actor{User: models.User{ID: repair.OwnerID}, Permissions: map[string]struct{}{authz.PermissionMediaLibrariesRead: {}}}
	page, err := s.RepairItems(actor, repair.LibraryID, repair.ID, 1, 50)
	if err != nil || page.Total != 2 || len(page.List) != 2 || page.List[0].SourceRelative != "b.mkv" || page.List[1].SourceRelative != "d.mkv" {
		t.Fatalf("failure page=%+v err=%v", page, err)
	}
	if _, err := s.RepairItems(actor, repair.LibraryID+1, repair.ID, 1, 50); ErrorCode(err) != CodeNotFound {
		t.Fatalf("cross-library repair items error=%v", err)
	}
	if _, err := s.RepairItems(actor, repair.LibraryID, repair.ID, 100001, 50); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("unbounded repair page error=%v", err)
	}
	secondBackend := &checkpointStructureBackend{fail: map[string]bool{}}
	second := s.executeStructureRepairItems(context.Background(), fastScanTestRuntime{}, repair, plan, StructureBoundary{}, secondBackend, nil)
	if second.GlobalErr != nil || second.Succeeded != 4 || second.Failed != 0 || second.Blocked != 0 {
		t.Fatalf("second=%+v", second)
	}
	if !reflect.DeepEqual(secondBackend.calls, []string{"b.mkv", "d.mkv"}) {
		t.Fatalf("retry replayed success: %v", secondBackend.calls)
	}
}

func TestStructureRepairCheckpointRejectsPlanMismatch(t *testing.T) {
	s, repair, _, _, _ := catalogStructureRepairFixture(t)
	plan := StructurePlan{Version: 1, LibraryID: repair.LibraryID, Items: []StructurePlanItem{{Kind: "video", SourceRelative: "a.mkv", TargetRelative: "x.mkv"}}}
	raw, _ := json.Marshal(plan)
	repair.PlanJSON = string(raw)
	if err := s.db.Model(&models.MediaLibraryStructureRepair{}).Where("id = ?", repair.ID).Update("plan_json", repair.PlanJSON).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.ensureStructureRepairItems(context.Background(), repair, plan, models.StorageTypeLocal, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&models.MediaLibraryStructureRepairItem{}).Where("repair_id = ?", repair.ID).Update("target_relative", "tampered.mkv").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.ensureStructureRepairItems(context.Background(), repair, plan, models.StorageTypeLocal, nil); err == nil {
		t.Fatal("tampered checkpoint was accepted")
	}
}

func TestStructureRepairCheckpointsRunLaterDependencyBeforeDependentItem(t *testing.T) {
	s, repair, _, _, _ := catalogStructureRepairFixture(t)
	plan := StructurePlan{Version: 1, LibraryID: repair.LibraryID, Items: []StructurePlanItem{
		{Kind: "video", SourceRelative: "a.mkv", TargetRelative: "b.mkv"},
		{Kind: "video", SourceRelative: "b.mkv", TargetRelative: "c.mkv"},
	}}
	raw, _ := json.Marshal(plan)
	repair.PlanJSON, repair.TotalItems = string(raw), len(plan.Items)
	if err := s.db.Model(&models.MediaLibraryStructureRepair{}).Where("id = ?", repair.ID).Updates(map[string]any{"plan_json": repair.PlanJSON, "total_items": repair.TotalItems}).Error; err != nil {
		t.Fatal(err)
	}
	backend := &checkpointStructureBackend{fail: map[string]bool{}}
	result := s.executeStructureRepairItems(context.Background(), fastScanTestRuntime{}, repair, plan, StructureBoundary{Storage: models.Storage{Type: models.StorageTypeLocal}}, backend, nil)
	if result.GlobalErr != nil || result.Succeeded != 2 || result.Failed != 0 || result.Blocked != 0 {
		t.Fatalf("result=%+v", result)
	}
	if !reflect.DeepEqual(backend.calls, []string{"b.mkv", "a.mkv"}) {
		t.Fatalf("dependency order=%v", backend.calls)
	}
}

func TestStructureRepairFailureSeparatesProviderTimeoutFromWorkerCancellation(t *testing.T) {
	providerTimeout := cloudpkg.Error(cloudpkg.CodeUnavailable, true, context.DeadlineExceeded)
	if code, _, global, retryAt := structureRepairFailure(context.Background(), providerTimeout); code != CodeMediaLibraryStructureApplyFailed || global || retryAt != nil {
		t.Fatalf("provider timeout should remain item-local: code=%q global=%v retry_at=%v", code, global, retryAt)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if code, _, global, _ := structureRepairFailure(canceled, context.Canceled); code != CodeMediaLibraryStructureBoundaryChanged || !global {
		t.Fatalf("worker cancellation should stop the batch: code=%q global=%v", code, global)
	}
}

func TestStructureRepairBatchCheckpointsScaleWithProviderBatches(t *testing.T) {
	s, repair, _, _, _ := catalogStructureRepairFixture(t)
	plan := StructurePlan{Version: 1, LibraryID: repair.LibraryID, Items: make([]StructurePlanItem, 235)}
	for index := range plan.Items {
		plan.Items[index] = StructurePlanItem{Kind: "video", SourceRelative: fmt.Sprintf("old/%03d.mkv", index), TargetRelative: fmt.Sprintf("new/%03d.mkv", index)}
	}
	raw, _ := json.Marshal(plan)
	repair.PlanJSON, repair.TotalItems = string(raw), len(plan.Items)
	if err := s.db.Model(&models.MediaLibraryStructureRepair{}).Where("id = ?", repair.ID).Updates(map[string]any{"plan_json": repair.PlanJSON, "total_items": repair.TotalItems}).Error; err != nil {
		t.Fatal(err)
	}
	var aggregateUpdates atomic.Int64
	callbackName := "test:count-structure-batch-checkpoints"
	if err := s.db.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "media_library_structure_repairs" {
			aggregateUpdates.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.db.Callback().Update().Remove(callbackName) })
	backend := &batchCheckpointStructureBackend{}
	result := s.executeStructureRepairItems(context.Background(), fastScanTestRuntime{}, repair, plan, StructureBoundary{Storage: models.Storage{Type: models.StorageTypePan115}}, backend, nil)
	if result.GlobalErr != nil || result.Succeeded != len(plan.Items) {
		t.Fatalf("result=%+v", result)
	}
	if !reflect.DeepEqual(backend.batches, []int{100, 100, 35}) {
		t.Fatalf("backend batches=%v", backend.batches)
	}
	// Each batch writes its in-flight summary, clears it after the external
	// call, and checkpoints verified outcomes once. No per-item transactions.
	if got := aggregateUpdates.Load(); got != 9 {
		t.Fatalf("aggregate checkpoint updates=%d want=9", got)
	}
}
