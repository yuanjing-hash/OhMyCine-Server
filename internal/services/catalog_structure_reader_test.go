package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
)

func TestCatalogStructureIssuePagePinsManualIdentityAndPoster(t *testing.T) {
	store, library, recognition, entries := catalogFixture(t)
	catalogConvert(t, store, library, recognition, entries)
	s := NewMediaLibraryStructureService(store.writeDB, NewAuditService(store.writeDB), nil, nil, zerolog.Nop())
	s.SetCatalogSnapshotStore(store)
	actor := Actor{Permissions: map[string]struct{}{authz.PermissionMediaLibrariesRead: {}}}
	row := models.MediaLibraryStructureIssue{Token: "sample-issue", LibraryID: library.ID, DiagnosisJobID: "diagnosis", Generation: 1, Code: "media_unrecognized", Kind: "video", State: "unrecognized", Title: "Old wrong title", CurrentPath: entries[0].RelativePath, RecognitionID: &recognition.ID, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := store.writeDB.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	err := s.withCatalogRead(context.Background(), library.ID, func(tx *gorm.DB, reader *CatalogReader) error {
		old, err := s.structureIssuesTx(tx, reader, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 50})
		if err != nil || old.Total != 1 {
			t.Fatalf("initial=%+v %v", old, err)
		}
		c, token := catalogCandidate(t, store, library, "delta", 1)
		recognition.Title, recognition.ManualOverride = "Correct series", true
		recognition.MetadataJSON, err = marshalRecognitionMetadata(MediaRecognitionResult{Snapshot: tmdb.Snapshot{Version: 1, TMDBID: *recognition.TMDBID, MediaType: "tv", Title: recognition.Title, PosterPath: "/correct.jpg"}})
		if err != nil {
			return err
		}
		if err := store.AppendBatch(context.Background(), c.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(recognition)}}); err != nil {
			return err
		}
		catalogPublish(t, store, c, token)
		again, err := s.structureIssuesTx(tx, reader, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 50})
		if err == nil && (again.Total != old.Total || again.List[0].Title != old.List[0].Title || again.List[0].PosterPath != old.List[0].PosterPath) {
			t.Fatal("issue page mixed catalog heads")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{})
	if err != nil || page.Total != 1 || page.List[0].Title != "Correct series" || page.List[0].PosterPath != "/correct.jpg" || page.List[0].RecognitionToken == "" {
		t.Fatalf("corrected page=%+v %v", page, err)
	}
	facts, err := s.loadStructureCatalog(context.Background(), library.ID)
	if err != nil || len(facts.Entries) != 2 || facts.Entries[0].SeriesTitle != "Correct series" || facts.Entries[0].Season == nil || *facts.Entries[0].Season != 2 || *facts.Entries[0].Episode != 1 {
		t.Fatalf("planner facts=%+v %v", facts, err)
	}
	var refs int64
	if err := store.writeDB.Model(&models.CatalogSnapshotReference{}).Where("owner_kind=?", "diagnosis").Count(&refs).Error; err != nil || refs != 0 {
		t.Fatalf("read reference leak=%d %v", refs, err)
	}
	withoutStore := NewMediaLibraryStructureService(store.writeDB, nil, nil, nil, zerolog.Nop())
	if _, err := withoutStore.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{}); !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("no-store anchor fallback=%v", err)
	}
}

func TestCatalogStructureConflictSafetyUsesEffectiveFacts(t *testing.T) {
	store, library, recognition, entries := catalogFixture(t)
	catalogConvert(t, store, library, recognition, entries)
	s := NewMediaLibraryStructureService(store.writeDB, nil, nil, nil, zerolog.Nop())
	s.SetCatalogSnapshotStore(store)
	plan := StructurePlan{LibraryID: library.ID, Items: []StructurePlanItem{{SourceRelative: entries[0].RelativePath, ProviderID: entries[0].ProviderID}}}
	if err := s.validateStructureSelectionSafety(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	before, err := s.loadStructureCatalog(context.Background(), library.ID)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := entries[1]
	duplicate.ProviderID = entries[0].ProviderID
	c, token := catalogCandidate(t, store, library, "delta", 1)
	if err := store.AppendBatch(context.Background(), c.ID, token, CatalogFactBatch{Entries: []models.CatalogEntryFact{CatalogEntryFromLegacy(duplicate, &recognition)}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, c, token)
	if err := s.validateStructureSelectionSafety(context.Background(), plan); ErrorCode(err) != CodeConflict {
		t.Fatalf("effective duplicate not rejected: %v", err)
	}
	// This publication changes logical content, unlike a head-only compaction.
	if err := store.writeDB.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).UpdateColumn("content_revision", gorm.Expr("content_revision + 1")).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.writeDB.Transaction(func(tx *gorm.DB) error { return validateStructureCatalogTx(tx, before) }); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("stale planning input accepted: %v", err)
	}
	c2, token2 := catalogCandidate(t, store, library, "delta", 2)
	if err := store.AppendBatch(context.Background(), c2.ID, token2, CatalogFactBatch{Entries: []models.CatalogEntryFact{{MediaLibraryEntry: duplicate, Tombstone: true}}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, c2, token2)
	if err := s.validateStructureSelectionSafety(context.Background(), plan); err != nil {
		t.Fatalf("tombstone duplicate resurrected: %v", err)
	}
}
