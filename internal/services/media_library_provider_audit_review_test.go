package services

import (
	"fmt"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestProviderAuditReviewSeparatesSourceAndManualSupersession(t *testing.T) {
	_, db, library, storage, _, _ := deletionFixture(t)
	source := catalogSourceFingerprint(library, storage)
	cutoff := time.Now().UTC()
	type item struct {
		source   string
		future   bool
		expected string
	}
	tests := []item{{source, false, "reconciled_by_scan"}, {"", false, "superseded_by_manual_audit"}, {"foreign", false, providerEventNeedsReview}, {source, true, providerEventNeedsReview}}
	ids := []uint{}
	for i, test := range tests {
		eventTime := cutoff.Add(-time.Minute)
		if test.future {
			eventTime = cutoff.Add(time.Minute)
		}
		inbox := models.ProviderEvent{ConnectionID: *storage.ConnectionID, Stream: "audit", ProviderEventID: string(rune('a' + i)), EventTime: eventTime, Kind: "deleted", ItemID: "private"}
		if err := db.Create(&inbox).Error; err != nil {
			t.Fatal(err)
		}
		row := models.MediaLibraryProviderEvent{LibraryID: library.ID, InboxEventID: inbox.ID, SourceFingerprint: test.source, ResolutionCode: providerEventNeedsReview, CreatedAt: cutoff.Add(-time.Minute)}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
		ids = append(ids, row.ID)
	}
	var after uint
	if err := db.Transaction(func(tx *gorm.DB) error {
		return resolveCoveredProviderEventsPageTx(tx, library.ID, source, cutoff, "audit", false, &after)
	}); err != nil {
		t.Fatal(err)
	}
	var unknown models.MediaLibraryProviderEvent
	if err := db.First(&unknown, ids[1]).Error; err != nil || unknown.ProcessedAt != nil {
		t.Fatal("automatic audit superseded unknown-source hint", err)
	}
	after = 0
	if err := db.Transaction(func(tx *gorm.DB) error {
		return resolveCoveredProviderEventsPageTx(tx, library.ID, source, cutoff, "audit", true, &after)
	}); err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		var row models.MediaLibraryProviderEvent
		if err := db.First(&row, id).Error; err != nil {
			t.Fatal(err)
		}
		if row.ResolutionCode != tests[i].expected {
			t.Fatalf("case%d code=%s expected=%s", i, row.ResolutionCode, tests[i].expected)
		}
	}
}

func TestProviderAuditReviewRechecksSourceBetweenPages(t *testing.T) {
	_, db, library, storage, _, _ := deletionFixture(t)
	library.BaselineGeneration = 1
	if err := db.Save(&library).Error; err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := db.First(&profile, library.ProfileID).Error; err != nil {
		t.Fatal(err)
	}
	old := library
	old.DirtyGeneration = 0
	cutoff := time.Now().UTC().Add(time.Second)
	scan := models.MediaLibraryScanRun{LibraryID: library.ID, Kind: "full", Status: "success", Generation: 1, SourceFingerprint: mediaLibraryScanSourceFingerprint(old, storage, profile), StartedAt: cutoff, CatalogPublishedAt: &cutoff}
	if err := db.Create(&scan).Error; err != nil {
		t.Fatal(err)
	}
	inbox := make([]models.ProviderEvent, 251)
	for i := range inbox {
		inbox[i] = models.ProviderEvent{ConnectionID: *storage.ConnectionID, Stream: "pagedaudit", ProviderEventID: fmt.Sprint(i), EventTime: cutoff.Add(-time.Minute), Kind: "deleted", ItemID: "private"}
	}
	if err := db.CreateInBatches(&inbox, 100).Error; err != nil {
		t.Fatal(err)
	}
	rows := make([]models.MediaLibraryProviderEvent, len(inbox))
	for i := range rows {
		rows[i] = models.MediaLibraryProviderEvent{LibraryID: library.ID, InboxEventID: inbox[i].ID, SourceFingerprint: catalogSourceFingerprint(library, storage), ResolutionCode: providerEventNeedsReview, CreatedAt: cutoff.Add(-time.Minute)}
	}
	if err := db.CreateInBatches(&rows, 100).Error; err != nil {
		t.Fatal(err)
	}
	pages := 0
	if err := resolveCoveredProviderDeletions(db, library.ID, scan.ID, func(tx *gorm.DB) (bool, error) {
		pages++
		if pages == 2 {
			return true, tx.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Update("provider_root_id", "changed-source").Error
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	var settled, pending int64
	if err := db.Model(&models.MediaLibraryProviderEvent{}).Where("library_id = ? AND resolution_code = ?", library.ID, "reconciled_by_scan").Count(&settled).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.MediaLibraryProviderEvent{}).Where("library_id = ? AND resolution_code = ?", library.ID, providerEventNeedsReview).Count(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if pages != 2 || settled != 250 || pending != 1 {
		t.Fatalf("pages=%d settled=%d pending=%d", pages, settled, pending)
	}
}
