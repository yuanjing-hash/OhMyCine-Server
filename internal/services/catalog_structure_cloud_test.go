package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
)

type structureDirectoryFailureDriver struct {
	*fakeMutationCloudDriver
	failName string
}

func (d *structureDirectoryFailureDriver) CreateDirectory(ctx context.Context, parent, name string) (cloudpkg.Item, error) {
	if name == d.failName {
		return cloudpkg.Item{}, errors.New("provider mkdir temporarily failed")
	}
	return d.fakeMutationCloudDriver.CreateDirectory(ctx, parent, name)
}

func TestCatalogStructureCloudDirectoryReceiptAndExactAssetParent(t *testing.T) {
	f := newPan115CatalogDeletionFixture(t, 1)
	if err := f.service.db.Model(&f.library).Updates(map[string]any{"strm_enabled": false, "signed_proxy_enabled": false}).Error; err != nil {
		t.Fatal(err)
	}
	asset := models.MediaLibrarySourceAsset{LibraryID: f.library.ID, RelativePath: "/Delete-A.zh.srt", ProviderID: "subtitle", ParentProviderID: f.library.ProviderRootID, Name: "Delete-A.zh.srt", Extension: ".srt", Size: 3, Active: true}
	if err := f.service.db.Create(&asset).Error; err != nil {
		t.Fatal(err)
	}
	f.driver.items[asset.ProviderID] = cloudpkg.Item{ID: asset.ProviderID, ParentID: asset.ParentProviderID, Name: asset.Name, Size: asset.Size}
	store := deletionSnapshotStore(t, f.service.db, f.library, f.entries, []models.MediaLibrarySourceAsset{asset})
	s := NewMediaLibraryStructureService(f.service.db, NewAuditService(f.service.db), nil, nil, zerolog.Nop())
	s.SetCatalogSnapshotStore(store)
	s.SetCatalogPublicationServices(NewMediaChangeService(s.db), nil)
	driver := &structureDirectoryFailureDriver{fakeMutationCloudDriver: f.driver, failName: "Season 02"}
	s.backends.Register(pan115MediaLibraryStructureBackend{driver: func(uint) (cloudpkg.Driver, error) { return driver, nil }})
	facts, err := s.loadStructureCatalog(context.Background(), f.library.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan := StructurePlan{Version: 1, LibraryID: f.library.ID, Generation: f.library.BaselineGeneration, RuleFingerprint: libraryRuleFingerprint(f.library), catalogFence: facts.LogicalFence, Items: []StructurePlanItem{
		{Kind: "video", SourceRelative: "Delete-A.mkv", TargetRelative: "New/Season 02/Correct.mkv", ProviderID: f.entries[0].ProviderID, Size: f.entries[0].Size},
		{Kind: "sidecar", SourceRelative: "Delete-A.zh.srt", TargetRelative: "New/Season 02/Correct.zh.srt", ProviderID: asset.ProviderID, Size: asset.Size},
	}}
	raw, _ := json.Marshal(plan)
	repair := models.MediaLibraryStructureRepair{ID: uuid.NewString(), OwnerID: f.actor.User.ID, LibraryID: f.library.ID, Scope: models.MediaLibraryStructureScopeWork, WorkKey: "cloud-work", RuleFingerprint: plan.RuleFingerprint, Generation: plan.Generation, PlanJSON: string(raw), StateJSON: "{}", Phase: "executing", TotalItems: 2, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := s.structureCatalogWriteTx(context.Background(), func(tx *gorm.DB) error {
		if err := s.freezeCatalogStructureRepairTx(tx, &repair, plan); err != nil {
			return err
		}
		return tx.Create(&repair).Error
	}); err != nil {
		t.Fatal(err)
	}
	result := s.runRepair(context.Background(), nil, repair.ID)
	if result.ErrorCode == "" || !strings.Contains(result.ErrorMessage, "媒体文件尚未移动") || driver.moveCalls != 0 {
		t.Fatalf("partial mkdir=%+v moves=%d", result, driver.moveCalls)
	}
	var receipts []models.CatalogStructureDirectoryReceipt
	if err := s.db.Where("repair_id = ?", repair.ID).Find(&receipts).Error; err != nil || len(receipts) != 1 || receipts[0].TargetRelative != "New" {
		t.Fatalf("receipts=%+v %v", receipts, err)
	}
	driver.failName = ""
	if result := s.runRepair(context.Background(), nil, repair.ID); result.ErrorCode != "" {
		t.Fatalf("resume=%+v", result)
	}
	var current models.MediaLibrarySourceAsset
	if err := store.Read(context.Background(), []uint{f.library.ID}, func(r *CatalogReader) error { return r.SourceAssets().Where("id = ?", asset.ID).First(&current).Error }); err != nil {
		t.Fatal(err)
	}
	physical := driver.items[asset.ProviderID]
	if current.Name != "Correct.zh.srt" || current.RelativePath != "/New/Season 02/Correct.zh.srt" || current.ParentProviderID != physical.ParentID || physical.Name != current.Name {
		t.Fatalf("asset=%+v physical=%+v", current, physical)
	}
	var remaining int64
	if err := s.db.Model(&models.CatalogStructureDirectoryReceipt{}).Where("repair_id = ?", repair.ID).Count(&remaining).Error; err != nil || remaining != 0 {
		t.Fatalf("completed receipts=%d %v", remaining, err)
	}
}
