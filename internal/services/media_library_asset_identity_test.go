package services

import (
	"context"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestRemoteAssetRenamePreservesIdentity(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	library, err := service.Create(context.Background(), actor, testLibraryInput("rename", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	other := models.MediaLibrary{Name: "other", NameNormalized: "other", StorageID: storage.ID, ProfileID: profile.ID, RelativeRoot: "/other"}
	err = db.Create(&other).Error
	if err != nil {
		t.Fatal(err)
	}
	rows := []models.MediaLibrarySourceAsset{
		{LibraryID: library.ID, RelativePath: "/Old/sub.ass", ProviderID: "subtitle", Active: true},
		{LibraryID: library.ID, RelativePath: "/Old/poster.jpg", ProviderID: "poster", Active: true},
		{LibraryID: library.ID, RelativePath: "/Untouched/poster.jpg", ProviderID: "other", Active: true},
		{LibraryID: other.ID, RelativePath: "/Old/sub.ass", ProviderID: "subtitle", Active: true},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	apply := func(sources []medialibrary.SourceAsset) {
		t.Helper()
		if err := db.Transaction(func(tx *gorm.DB) error {
			var existing []models.MediaLibrarySourceAsset
			if err := tx.Where("library_id = ?", library.ID).Find(&existing).Error; err != nil {
				return err
			}
			_, err := matchRenamedSourceAssetsTx(tx, library.ID, existing, sources)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	apply([]medialibrary.SourceAsset{{ProviderID: "subtitle", RelativePath: "/New/sub.ass"}, {ProviderID: "poster", RelativePath: "/New/poster.jpg"}})
	apply([]medialibrary.SourceAsset{{ProviderID: "subtitle", RelativePath: "/New/renamed.ass"}})
	var actual models.MediaLibrarySourceAsset
	if err := db.First(&actual, rows[0].ID).Error; err != nil || actual.RelativePath != "/New/renamed.ass" {
		t.Fatalf("row=%+v err=%v", actual, err)
	}
	var count int64
	if err := db.Model(&models.MediaLibrarySourceAsset{}).Count(&count).Error; err != nil || count != 4 {
		t.Fatalf("unrelated rows changed: count=%d err=%v", count, err)
	}
	var untouched models.MediaLibrarySourceAsset
	if err := db.First(&untouched, rows[3].ID).Error; err != nil || untouched.RelativePath != "/Old/sub.ass" {
		t.Fatalf("other library changed: %+v %v", untouched, err)
	}
	// A conflicting target must not overwrite another file's identity.
	err = db.Transaction(func(tx *gorm.DB) error {
		var existing []models.MediaLibrarySourceAsset
		if err := tx.Where("library_id = ?", library.ID).Find(&existing).Error; err != nil {
			return err
		}
		_, err := matchRenamedSourceAssetsTx(tx, library.ID, existing, []medialibrary.SourceAsset{{ProviderID: "subtitle", RelativePath: "/Untouched/poster.jpg"}})
		return err
	})
	if err == nil {
		t.Fatal("expected identity collision rejection")
	}
	if err := db.First(&actual, rows[0].ID).Error; err != nil || actual.RelativePath != "/New/renamed.ass" {
		t.Fatalf("collision changed source: %+v %v", actual, err)
	}
}

func TestStructureDiagnosisFindsIdentityAliasesAcrossTargets(t *testing.T) {
	candidates := []structurePlanCandidate{
		{kind: "sidecar", source: "Old/poster.jpg", target: "TargetA/poster.jpg", providerID: "same"},
		{kind: "sidecar", source: "New/poster.jpg", target: "TargetB/poster.jpg", providerID: "same"},
		{kind: "sidecar", source: "Unassociated/poster.jpg", providerID: "same"},
		{kind: "sidecar", source: "Other/poster.jpg", target: "OtherNew/poster.jpg", providerID: "other"},
	}
	plan := StructurePlan{}
	appendStructureCandidates(&plan, candidates)
	if len(plan.Items) != 1 || plan.Items[0].ProviderID != "other" {
		t.Fatalf("unsafe moves: %+v", plan.Items)
	}
	if plan.Classifications.CatalogDuplicateConflict != 3 {
		t.Fatalf("duplicate aliases not diagnosed: %+v", plan)
	}
}

func TestEventAssetRenameReconcile(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	library, err := service.Create(context.Background(), actor, testLibraryInput("event rename", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.Storage{}).Where("id = ?", storage.ID).Update("type", models.StorageTypePan115).Error; err != nil {
		t.Fatal(err)
	}
	asset := models.MediaLibrarySourceAsset{LibraryID: library.ID, RelativePath: "/Old/poster.jpg", ProviderID: "poster", Active: true}
	if err := db.Create(&asset).Error; err != nil {
		t.Fatal(err)
	}
	result := medialibrary.Result{Partial: true, Scoped: true, Assets: []medialibrary.SourceAsset{{ProviderID: "poster", RelativePath: "/New/poster.jpg", Name: "poster.jpg", Extension: "jpg"}}}
	ctx := withProviderChangeScope(context.Background(), providerChangeScope{ParentIDs: []string{"parent"}, VerifiedResult: &result})
	if _, err := service.reconcile(ctx, library.ID, "event"); err != nil {
		t.Fatal(err)
	}
	var rows []models.MediaLibrarySourceAsset
	if err := db.Where("library_id = ?", library.ID).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != asset.ID || rows[0].RelativePath != "/New/poster.jpg" {
		t.Fatalf("rename left aliases: %+v", rows)
	}

	// A subsequent filename change still updates the same row.
	result.Assets[0].RelativePath = "/New/renamed.jpg"
	result.Assets[0].Name = "renamed.jpg"
	if _, err := service.reconcile(ctx, library.ID, "event"); err != nil {
		t.Fatal(err)
	}
	if err := db.Where("library_id = ?", library.ID).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != asset.ID || rows[0].RelativePath != "/New/renamed.jpg" {
		t.Fatalf("second rename left aliases: %+v", rows)
	}
}

func TestStructureAliasTargetRemainsBlockedForOtherFiles(t *testing.T) {
	plan := StructurePlan{}
	appendStructureCandidates(&plan, []structurePlanCandidate{
		{kind: "sidecar", source: "Old/poster.jpg", target: "Final/poster.jpg", providerID: "same"},
		{kind: "sidecar", source: "New/poster.jpg", target: "Other/poster.jpg", providerID: "same"},
		{kind: "sidecar", source: "Third/poster.jpg", target: "Final/poster.jpg", providerID: "third"},
	})
	if len(plan.Items) != 0 {
		t.Fatalf("ambiguous target became actionable: %+v", plan.Items)
	}
}
