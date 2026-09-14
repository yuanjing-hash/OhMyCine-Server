package services

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
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

func TestLibraryRetirementWaitsForEnteredOwnerAndDiscardsQuiescentEvidence(t *testing.T) {
	for _, state := range []string{"entered", "quiescent"} {
		t.Run(state, func(t *testing.T) {
			s, library, actor := retirementFixture(t)
			proof := models.CatalogPhysicalWrite{LibraryID: library.ID, OwnerKind: CatalogPhysicalDeletion, OwnerID: "unresolved-delete", State: state, Revision: 1, OwnerDigest: "test", EnteredAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
			if err := s.db.Create(&proof).Error; err != nil {
				t.Fatal(err)
			}
			claim, row := claimRetirement(t, s, library, actor)
			worker := NewMediaLibraryRetirementWorker(s)
			waiting := false
			for i := 0; i < 8 && row.Phase != "completed"; i++ {
				var err error
				waiting, _, err = worker.step(context.Background(), claim, &row)
				if err != nil {
					t.Fatal(err)
				}
			}
			if state == "entered" {
				if !waiting || row.Phase != "draining" {
					t.Fatalf("entered owner did not keep retirement waiting: phase=%s waiting=%v", row.Phase, waiting)
				}
				if err := s.db.First(&proof, proof.ID).Error; err != nil || proof.State != state {
					t.Fatal("entered evidence was removed before external I/O exit")
				}
				// The credential is not retained as history. Once the actual caller
				// has left its external-I/O boundary, retirement removes it together
				// with every other selected-library execution record.
				if err := s.db.Model(&models.CatalogPhysicalWrite{}).Where("id = ?", proof.ID).Updates(map[string]any{"state": "quiescent", "updated_at": time.Now().UTC()}).Error; err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 100 && row.Phase != "completed"; i++ {
				if _, _, err := worker.step(context.Background(), claim, &row); err != nil {
					t.Fatal(err)
				}
			}
			if row.Phase != "completed" {
				t.Fatalf("quiescent retirement did not finish: %s", row.Phase)
			}
			if err := s.db.First(&proof, proof.ID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
				t.Fatalf("quiescent evidence remained after deletion: %v", err)
			}
		})
	}
}
