package services

import (
	"context"
	"time"

	"github.com/rs/zerolog"
	"gorm.io/gorm"
)

// This is only an execution adapter for the durable shared ledger. It neither
// invents an in-memory lock nor treats a revoked queue lease as stopped I/O.
func enterCatalogPhysicalWrite(ctx context.Context, db *gorm.DB, input CatalogPhysicalWriteInput) (CatalogPhysicalWritePermit, error) {
	var permit CatalogPhysicalWritePermit
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		permit, err = EnterCatalogPhysicalWriteTx(tx, input)
		return err
	})
	return permit, err
}

// Defer only at a boundary that joins every physical call. This acknowledges
// execution exit, NOT success: unresolved work continues blocking conversion.
func quiesceCatalogPhysicalWrite(db *gorm.DB, permit CatalogPhysicalWritePermit, log zerolog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return QuiesceCatalogPhysicalWriteTx(tx, permit) }); err != nil {
		log.Warn().Uint("library_id", permit.evidence.LibraryID).Str("owner_kind", permit.evidence.OwnerKind).Str("error_code", "physical_write_exit_pending").Msg("文件操作已退出，持久化退出确认失败；恢复保护继续保留")
	}
}
