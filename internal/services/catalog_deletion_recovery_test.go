package services

import (
	"context"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestCatalogDeletionFreshPreviewResumesSamePhysicalOwner(t *testing.T) {
	for _, versioned := range []bool{false, true} {
		t.Run(map[bool]string{true: "versioned", false: "legacy"}[versioned], func(t *testing.T) {
			f := newPan115CatalogDeletionFixture(t, 2)
			if versioned {
				f.service.SetCatalogSnapshotStore(deletionSnapshotStore(t, f.service.db, f.library, f.entries, nil))
			}
			first := f.preview(t)
			f.driver.recycleFailID = f.entries[1].ProviderID
			if _, err := f.service.ConfirmCatalogDeletion(context.Background(), f.actor, f.library.ID, f.work, first.ConfirmationToken, RequestContext{}); ErrorCode(err) != CodeMediaCatalogDeletionPartial {
				t.Fatalf("partial %v", err)
			}
			var proof models.CatalogPhysicalWrite
			if err := f.service.db.Where("owner_kind=?", CatalogPhysicalDeletion).First(&proof).Error; err != nil || proof.State != "quiescent" {
				t.Fatalf("missing quiescence %+v %v", proof, err)
			}
			retry := f.preview(t)
			if first.ConfirmationToken == retry.ConfirmationToken {
				t.Fatal("confirmation was not renewed")
			}
			var count int64
			if err := f.service.db.Model(&models.CatalogPhysicalWrite{}).Where("owner_kind=?", CatalogPhysicalDeletion).Count(&count).Error; err != nil || count != 1 {
				t.Fatal("orphaned physical owner")
			}
			if _, err := f.service.ConfirmCatalogDeletion(context.Background(), f.actor, f.library.ID, f.work, first.ConfirmationToken, RequestContext{}); err == nil {
				t.Fatal("old token reused")
			}
			f.driver.recycleFailID = ""
			if _, err := f.service.ConfirmCatalogDeletion(context.Background(), f.actor, f.library.ID, f.work, retry.ConfirmationToken, RequestContext{}); err != nil {
				t.Fatal(err)
			}
			if err := f.service.db.Transaction(func(tx *gorm.DB) error { return AssertCatalogPhysicalDrainedTx(tx, f.library.ID) }); err != nil {
				t.Fatalf("successful retry left unresolved proof %v", err)
			}
		})
	}
}
