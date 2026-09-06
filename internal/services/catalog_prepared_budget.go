package services

import (
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// Verify derived collection rows as well as E/R/A rows before physical work.
// At most nine layer headers participate, independent of catalog size.
func ValidateCatalogPreparedDeltaBudgetTx(tx *gorm.DB, candidateID string) error {
	var prepared models.CatalogSnapshot
	if err := tx.First(&prepared, "id = ? AND state = ?", candidateID, "ready").Error; err != nil {
		return err
	}
	var totals struct{ Rows, Bytes, Count int64 }
	if err := tx.Table("catalog_head_layers AS l").Joins("JOIN catalog_snapshots AS s ON s.id=l.snapshot_id").Where("l.library_id=? AND l.rank>0", prepared.LibraryID).Select("COALESCE(SUM(s.row_count),0) AS rows, COALESCE(SUM(s.byte_count),0) AS bytes, COUNT(*) AS count").Scan(&totals).Error; err != nil {
		return err
	}
	if totals.Count >= CatalogMaxDeltas || totals.Rows+prepared.RowCount > CatalogMaxDeltaRows || totals.Bytes+prepared.ByteCount > CatalogMaxDeltaBytes {
		return ErrCatalogBudget
	}
	return nil
}
