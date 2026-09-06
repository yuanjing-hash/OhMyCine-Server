package services

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestCatalogIdentityBatchBudgetAdaptsWithinBounds(t *testing.T) {
	var budget catalogIdentityBatchBudget
	if budget.limit() != 32 {
		t.Fatal("allocation starts with an oversized batch")
	}
	budget.observe(32, 120*time.Millisecond)
	if budget.limit() != 16 {
		t.Fatal("slow allocation did not reduce writer batch")
	}
	for i := 0; i < 100; i++ {
		budget.observe(budget.limit(), time.Millisecond)
	}
	if budget.limit() != 128 {
		t.Fatal("fast allocation exceeded bounded maximum")
	}
	for i := 0; i < 100; i++ {
		budget.observe(budget.limit(), time.Second)
	}
	if budget.limit() != 8 {
		t.Fatal("slow allocation cannot make forward progress")
	}
}

func TestCatalogIdentityRenameAndOldPathReuse(t *testing.T) {
	s, library, rec, entries := catalogFixture(t)
	catalogConvert(t, s, library, rec, entries)
	c, token := catalogCandidate(t, s, library, "delta", 1)
	original := entries[0]
	renamed := original
	renamed.RelativePath = "Show/Season 02/01.renamed.mkv"
	requests := []CatalogIdentityRequest{{Kind: "entry", SourceKey: renamed.RelativePath, ProviderID: original.ProviderID, ExistingID: original.ID}}
	ids, err := s.ResolveIdentities(context.Background(), c.ID, token, requests)
	if err != nil || len(ids) != 1 || ids[0] != original.ID {
		t.Fatalf("rename IDs=%v: %v", ids, err)
	}
	if err := s.AppendBatch(context.Background(), c.ID, token, CatalogFactBatch{Entries: []models.CatalogEntryFact{CatalogEntryFromLegacy(renamed, &rec)}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, s, c, token)
	current := catalogReadEntries(t, s, library.ID, "id=?", original.ID)
	if len(current) != 1 || current[0].RelativePath != renamed.RelativePath {
		t.Fatalf("rename=%+v", current)
	}
	var anchor models.MediaLibraryEntry
	if err := s.writeDB.First(&anchor, original.ID).Error; err != nil {
		t.Fatal(err)
	}
	if anchor.RelativePath != original.RelativePath {
		t.Fatal("rename changed immutable legacy anchor")
	}

	c2, token2 := catalogCandidate(t, s, library, "delta", 2)
	newRequest := CatalogIdentityRequest{Kind: "entry", SourceKey: original.RelativePath}
	newIDs, err := s.ResolveIdentities(context.Background(), c2.ID, token2, []CatalogIdentityRequest{newRequest, newRequest})
	if err != nil || len(newIDs) != 2 || newIDs[0] == original.ID || newIDs[0] != newIDs[1] {
		t.Fatalf("old-path reuse=%v: %v", newIDs, err)
	}
	retried, err := s.ResolveIdentities(context.Background(), c2.ID, token2, []CatalogIdentityRequest{newRequest})
	if err != nil || retried[0] != newIDs[0] {
		t.Fatalf("retry=%v: %v", retried, err)
	}
	replacement := original
	replacement.ID, replacement.ProviderID = newIDs[0], "different-file"
	if err := s.AppendBatch(context.Background(), c2.ID, token2, CatalogFactBatch{Entries: []models.CatalogEntryFact{CatalogEntryFromLegacy(replacement, &rec)}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, s, c2, token2)
	visible := catalogReadEntries(t, s, library.ID, "")
	if len(visible) != 3 {
		t.Fatalf("lost versions: %+v", visible)
	}
	for _, row := range visible {
		if strings.Contains(row.RelativePath, ".omc-catalog-anchor") {
			t.Fatal("surrogate leaked")
		}
	}

	c3, token3 := catalogCandidate(t, s, library, "delta", 3)
	for _, invalid := range []CatalogIdentityRequest{
		{Kind: "entry", SourceKey: "wrong.mkv", ProviderID: "unrelated", ExistingID: original.ID},
		{Kind: "entry", SourceKey: original.RelativePath, ExistingID: original.ID},
		{Kind: "recognition", SourceKey: "different-show", ExistingID: rec.ID},
	} {
		if _, err := s.ResolveIdentities(context.Background(), c3.ID, token3, []CatalogIdentityRequest{invalid}); !errors.Is(err, ErrCatalogInvalid) {
			t.Fatalf("accepted false evidence %+v: %v", invalid, err)
		}
	}
	if err := s.AppendBatch(context.Background(), c3.ID, token3, CatalogFactBatch{Entries: []models.CatalogEntryFact{{MediaLibraryEntry: models.MediaLibraryEntry{ID: original.ID, LibraryID: library.ID}, Tombstone: true}}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, s, c3, token3)
	c4, token4 := catalogCandidate(t, s, library, "delta", 4)
	if _, err := s.ResolveIdentities(context.Background(), c4.ID, token4, requests); !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("retired ID revived: %v", err)
	}
}

func TestCatalogIdentityConvertingRequiresExactExistingIdentity(t *testing.T) {
	s, library, _, entries := catalogFixture(t)
	c, token := catalogCandidate(t, s, library, "base", 0)
	valid := CatalogIdentityRequest{Kind: "entry", SourceKey: entries[0].RelativePath, ExistingID: entries[0].ID}
	for i := 0; i < 2; i++ {
		ids, err := s.ResolveIdentities(context.Background(), c.ID, token, []CatalogIdentityRequest{valid, valid})
		if err != nil || len(ids) != 2 || ids[0] != valid.ExistingID || ids[1] != valid.ExistingID {
			t.Fatalf("conversion retry %v: %v", ids, err)
		}
	}
	for _, invalid := range []CatalogIdentityRequest{
		{Kind: "entry", SourceKey: valid.SourceKey},
		{Kind: "entry", SourceKey: "renamed.mkv", ProviderID: entries[0].ProviderID, ExistingID: valid.ExistingID},
		{Kind: "entry", SourceKey: valid.SourceKey, ExistingID: entries[1].ID},
	} {
		if _, err := s.ResolveIdentities(context.Background(), c.ID, token, []CatalogIdentityRequest{invalid}); !errors.Is(err, ErrCatalogInvalid) {
			t.Fatalf("accepted invalid conversion: %v", err)
		}
	}
}

func TestCatalogIdentityNewRecognitionAndAssetAnchorsRetainForeignKeys(t *testing.T) {
	s, library, rec, entries := catalogFixture(t)
	catalogConvert(t, s, library, rec, entries)
	c, token := catalogCandidate(t, s, library, "delta", 1)
	requests := []CatalogIdentityRequest{{Kind: "recognition", SourceKey: "new-work"}, {Kind: "asset", SourceKey: "new-work/poster.jpg"}}
	ids, err := s.ResolveIdentities(context.Background(), c.ID, token, requests)
	if err != nil || len(ids) != 2 {
		t.Fatalf("new foreign-key anchors=%v: %v", ids, err)
	}
	var recognition models.MediaLibraryRecognition
	if err := s.writeDB.First(&recognition, ids[0]).Error; err != nil {
		t.Fatal(err)
	}
	if recognition.ProfileID != library.ProfileID || !strings.HasPrefix(recognition.SourceKey, ".omc-catalog-anchor/") {
		t.Fatalf("invalid recognition anchor=%+v", recognition)
	}
	var asset models.MediaLibrarySourceAsset
	if err := s.writeDB.First(&asset, ids[1]).Error; err != nil {
		t.Fatal(err)
	}
	if asset.Active || !strings.HasPrefix(asset.RelativePath, ".omc-catalog-anchor/") {
		t.Fatalf("visible source asset anchor=%+v", asset)
	}
	again, err := s.ResolveIdentities(context.Background(), c.ID, token, requests)
	if err != nil || len(again) != 2 || again[0] != ids[0] || again[1] != ids[1] {
		t.Fatalf("allocation retry=%v: %v", again, err)
	}
	if len(catalogReadEntries(t, s, library.ID, "")) != 2 {
		t.Fatal("pending anchors changed published catalog")
	}
}
