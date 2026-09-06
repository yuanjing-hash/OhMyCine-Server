package services

import "gorm.io/gorm"

// Raw facts are for versioned writers only. Preserve masks when changing a
// physical path/identity; normal readers must continue to use Entries(), which
// overlays shared recognition metadata. These queries remain transaction-bound.
func (r *CatalogReader) RawEntryFacts() *gorm.DB {
	sql, args := r.effectiveSQL("catalog_entry_facts")
	return r.tx.Table("(?) AS catalog_raw_entries", r.tx.Raw(sql, args...)).Where("tombstone = ?", false)
}

func (r *CatalogReader) RawSourceAssetFacts() *gorm.DB {
	sql, args := r.effectiveSQL("catalog_source_asset_facts")
	return r.tx.Table("(?) AS catalog_raw_assets", r.tx.Raw(sql, args...)).Where("tombstone = ?", false)
}
