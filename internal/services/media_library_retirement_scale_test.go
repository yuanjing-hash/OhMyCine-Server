package services

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestLibraryRetirement100KBoundedWriters(t *testing.T) {
	if testing.Short() {
		t.Skip("100k retirement regression")
	}
	s, library, actor := retirementFixture(t)
	// One user/library with 100k logical media rows, not 100k broad library transactions.
	for start := 0; start < 100000; start += CatalogBatchRows {
		batch := make([]models.PlayerMediaFavorite, CatalogBatchRows)
		for i := range batch {
			batch[i] = models.PlayerMediaFavorite{UserID: actor.User.ID, LibraryID: library.ID, WorkKey: fmt.Sprintf("scale-%06d", start+i)}
		}
		if err := s.db.Create(&batch).Error; err != nil {
			t.Fatal(err)
		}
	}
	started := time.Now()
	claim, row := claimRetirement(t, s, library, actor)
	accepted := time.Since(started)
	maxWriter := time.Duration(0)
	for i := 0; i < 600 && row.Phase != "completed"; i++ {
		before := row.ProcessedRows
		start := time.Now()
		_, _, err := NewMediaLibraryRetirementWorker(s).step(context.Background(), claim, &row)
		elapsed := time.Since(start)
		if elapsed > maxWriter {
			maxWriter = elapsed
		}
		if err != nil {
			t.Fatal(err)
		}
		if row.ProcessedRows-before > CatalogBatchRows {
			t.Fatalf("unbounded cleanup %d", row.ProcessedRows-before)
		}
	}
	if row.Phase != "completed" || row.ProcessedRows < 100000 {
		t.Fatalf("unfinished %+v", row)
	}
	t.Logf("100k retirement accepted=%s total=%s longest writer=%s", accepted, time.Since(started), maxWriter)
}

func TestLibraryRetirementRealPhysicalGuardPreservesUnresolvedOwner(t *testing.T) {
	for _, state := range []string{"entered", "quiescent"} {
		t.Run(state, func(t *testing.T) {
			s, library, actor := retirementFixture(t)
			proof := models.CatalogPhysicalWrite{LibraryID: library.ID, OwnerKind: CatalogPhysicalDeletion, OwnerID: "unresolved-delete", State: state, Revision: 1, OwnerDigest: "test", EnteredAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
			if err := s.db.Create(&proof).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := s.DeleteRequest(context.Background(), actor, library.ID, RequestContext{}); err == nil {
				t.Fatal("unresolved physical owner admitted")
			}
			var count int64
			if err := s.db.Model(&models.MediaLibraryRetirement{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatal("retirement gate survived refusal")
			}
			var kept models.MediaLibrary
			if err := s.db.First(&kept, library.ID).Error; err != nil || kept.Enabled != library.Enabled {
				t.Fatal("library changed on refusal")
			}
			if err := s.db.First(&proof, proof.ID).Error; err != nil || proof.State != state {
				t.Fatal("unresolved owner lost")
			}
		})
	}
}
