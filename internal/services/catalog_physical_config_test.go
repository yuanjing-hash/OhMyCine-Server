package services

import (
	"encoding/json"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestCatalogPhysicalUnsettledConfigurationKeepsRecoveryFences(t *testing.T) {
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
	var library models.MediaLibrary
	var storage models.Storage
	var profile models.MediaClassificationProfile
	if err := db.First(&library, repair.LibraryID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&profile, library.ProfileID).Error; err != nil {
		t.Fatal(err)
	}
	nextLibrary := library
	nextLibrary.RelativeRoot = "changed-source"
	if err := db.Transaction(func(tx *gorm.DB) error {
		if _, err := applyCatalogLibraryChangeTx(tx, library, &nextLibrary, storage, storage, profile); err != nil {
			return err
		}
		return tx.Model(&library).Update("relative_root", nextLibrary.RelativeRoot).Error
	}); err == nil {
		t.Fatal("quiescent physical work allowed source replacement")
	}
	nextStorage := storage
	nextStorage.Enabled = false
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := applyCatalogStorageChangeTx(tx, storage, nextStorage); err != nil {
			return err
		}
		return tx.Model(&storage).Update("enabled", false).Error
	}); err == nil {
		t.Fatal("quiescent physical work allowed storage disable")
	}
	if err := db.First(&library, library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&storage, storage.ID).Error; err != nil {
		t.Fatal(err)
	}
	if library.RelativeRoot != "/" || !storage.Enabled {
		t.Fatal("refusal modified source or enabled state")
	}
	// Cosmetic names are not execution configuration and must remain editable.
	named := library
	named.Name = "display only"
	if err := db.Transaction(func(tx *gorm.DB) error {
		_, err := applyCatalogLibraryChangeTx(tx, library, &named, storage, storage, profile)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogPhysicalProfileRefusesBeforeGlobalRevisionCommit(t *testing.T) {
	db, repair, input := catalogPhysicalRepairFixture(t, false)
	var library models.MediaLibrary
	var original models.MediaClassificationProfile
	if err := db.First(&library, repair.LibraryID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&original, library.ProfileID).Error; err != nil {
		t.Fatal(err)
	}
	custom := original
	custom.ID = 0
	custom.Code = nil
	custom.Name = "physical custom"
	custom.NameNormalized = custom.Name
	custom.Kind = models.MediaClassificationProfileKindCustom
	custom.Protected = false
	if err := db.Create(&custom).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&library).Update("profile_id", custom.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { _, err := EnterCatalogPhysicalWriteTx(tx, input); return err }); err != nil {
		t.Fatal(err)
	}
	var user models.User
	if err := db.First(&user, repair.OwnerID).Error; err != nil {
		t.Fatal(err)
	}
	actor := Actor{User: user, Permissions: map[string]struct{}{authz.PermissionMediaClassificationProfilesUpdate: {}}}
	service := NewMediaClassificationProfileService(db, NewAuditService(db), nil)
	if _, err := service.Update(actor, custom.ID, UpdateMediaClassificationProfileInput{Revision: custom.Revision, Name: "changed", Rules: json.RawMessage(custom.RulesJSON)}, RequestContext{}); err == nil {
		t.Fatal("profile changed despite physical recovery owner")
	}
	var after models.MediaClassificationProfile
	if err := db.First(&after, custom.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.Revision != custom.Revision || after.Name != custom.Name {
		t.Fatal("global profile committed before refusing physical owner")
	}
}
