package services

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/storage"
	"gorm.io/gorm"
)

const (
	CatalogMaxPreparations               = 8
	CatalogMaxBasePreparations           = 2
	catalogForegroundSpaceReserve uint64 = 64 << 20
	catalogWALWorkingReserve      uint64 = 16 << 20
)

// Caps persist across processes/restarts. Expired candidates still occupy slots
// until fenced recovery abandons them; age alone never frees a live owner.
func catalogPreparationBudgetTx(tx *gorm.DB, input CatalogCandidateInput) error {
	var active []models.CatalogSnapshot
	if err := tx.Select("id", "kind", "library_id").Where("state IN ('building','validating','ready')").Limit(CatalogMaxPreparations).Find(&active).Error; err != nil {
		return err
	}
	if len(active) >= CatalogMaxPreparations {
		return fmt.Errorf("%w: preparation_concurrency", ErrCatalogBudget)
	}
	bases := 0
	for _, row := range active {
		if row.Kind == "base" {
			bases++
			if input.Kind == "base" && row.LibraryID == input.LibraryID {
				return fmt.Errorf("%w: library_base_preparation", ErrCatalogBudget)
			}
		}
	}
	if input.Kind == "base" && bases >= CatalogMaxBasePreparations {
		return fmt.Errorf("%w: base_preparation_concurrency", ErrCatalogBudget)
	}
	return nil
}

func catalogRequiredFreeBytes(growth int64) (uint64, error) {
	// Four times encoded facts estimates candidate pages plus indexes and WAL
	// growth; it is a conservative admission estimate, not a total-size promise.
	// Current/retired/pinned pages and existing WAL already consume disk free.
	const reserve = catalogForegroundSpaceReserve + catalogWALWorkingReserve
	if growth < 0 || uint64(growth) > (math.MaxUint64-reserve)/4 {
		return 0, ErrCatalogBudget
	}
	return reserve + 4*uint64(growth), nil
}

// Read-only and outside a writer transaction. Failure never logs DB paths or
// deletes data. Growth is checked again on every batch, including new libraries
// whose final size is not known in advance. No check can promise against another
// process filling the disk after admission; SQLITE_FULL remains an honest error.
func (s *CatalogSnapshotStore) checkCatalogSpace(ctx context.Context, growth int64) error {
	needed, err := catalogRequiredFreeBytes(growth)
	if err != nil {
		return err
	}
	if s.spaceAvailable != nil {
		free, err := s.spaceAvailable(ctx)
		if err != nil || free < needed {
			return fmt.Errorf("%w: storage_pressure", ErrCatalogBudget)
		}
		return nil
	}
	var databases []struct{ Name, File string }
	if err := s.readDB.WithContext(ctx).Raw("PRAGMA database_list").Scan(&databases).Error; err != nil {
		return err
	}
	for _, db := range databases {
		if db.Name != "main" {
			continue
		}
		if db.File == "" { // Explicit SQLite in-memory fixtures have no disk.
			return nil
		}
		probe := (storage.LocalDriver{}).ProbeRoot(filepath.Dir(db.File))
		if probe.FreeBytes == nil || *probe.FreeBytes < needed {
			return fmt.Errorf("%w: storage_pressure", ErrCatalogBudget)
		}
		return ctx.Err()
	}
	return fmt.Errorf("%w: storage_capacity_unknown", ErrCatalogBudget)
}

func (s *CatalogSnapshotStore) writeCatalogGrowth(ctx context.Context, growth int64, write func(*gorm.DB) error) error {
	return s.writeCatalogGrowthObserved(ctx, growth, write, nil)
}

func (s *CatalogSnapshotStore) writeCatalogGrowthObserved(ctx context.Context, growth int64, write func(*gorm.DB) error, observe func(time.Duration)) error {
	return s.admission.WithBackground(ctx, func() error {
		if err := s.checkCatalogSpace(ctx, growth); err != nil {
			return err
		}
		started := time.Now()
		err := s.writeDB.WithContext(ctx).Transaction(write)
		if observe != nil {
			observe(time.Since(started))
		}
		return err
	})
}

func (s *CatalogSnapshotStore) catalogCandidateGrowth(ctx context.Context, input CatalogCandidateInput) (int64, error) {
	if input.Kind != "base" {
		return CatalogBatchBytes, nil
	}
	var bytes int64
	err := s.readDB.WithContext(ctx).Raw(`SELECT COALESCE(SUM(s.byte_count),0) FROM catalog_snapshots s JOIN catalog_head_layers l ON l.snapshot_id=s.id WHERE l.library_id=?`, input.LibraryID).Scan(&bytes).Error
	return max(bytes, int64(CatalogBatchBytes)), err
}
