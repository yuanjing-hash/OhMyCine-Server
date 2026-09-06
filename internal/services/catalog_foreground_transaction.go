package services

import (
	"context"
	"gorm.io/gorm"
)

// Admission is acquired BEFORE beginning SQLite's writer transaction. This
// helper must never be called by code already holding an admitted transaction.
func withForegroundTransaction(ctx context.Context, db *gorm.DB, admission *CatalogWriteAdmission, write func(*gorm.DB) error) error {
	transaction := func() error { return db.WithContext(ctx).Transaction(write) }
	if admission == nil {
		return transaction()
	}
	return admission.WithForeground(ctx, transaction)
}

func withBackgroundTransaction(ctx context.Context, db *gorm.DB, admission *CatalogWriteAdmission, write func(*gorm.DB) error) error {
	transaction := func() error { return db.WithContext(ctx).Transaction(write) }
	if admission == nil {
		return transaction()
	}
	return admission.WithBackground(ctx, transaction)
}
