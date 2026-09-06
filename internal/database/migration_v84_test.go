package database

import (
	"strings"
	"testing"
)

func TestCatalogIdentityLookupV84UsesExactAnchorAndSourceIndexes(t *testing.T) {
	db := structureMigrationDB(t, 82)
	if err := migrateCatalogIdentityLookup(db); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"SELECT COUNT(*) FROM catalog_identities WHERE library_id=1 AND source_epoch=1 AND entity_kind='entry' AND anchor_id IN (1,2,3)",
		"SELECT * FROM catalog_identities WHERE library_id=1 AND source_epoch=1 AND entity_kind='entry' AND source_key IN ('a','b')",
	} {
		var rows []struct{ Detail string }
		if err := db.Raw("EXPLAIN QUERY PLAN " + query).Scan(&rows).Error; err != nil {
			t.Fatal(err)
		}
		found := false
		for _, row := range rows {
			if strings.Contains(row.Detail, "anchor_id=?") || strings.Contains(row.Detail, "source_key=?") {
				found = true
			}
		}
		if !found {
			t.Fatalf("lookup scans library prefix: %+v", rows)
		}
	}
}
