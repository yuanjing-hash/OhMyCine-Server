package services

import (
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func assertReorganizationPhysicalState(t *testing.T, db *gorm.DB, id, state string) {
	t.Helper()
	var proof models.CatalogPhysicalWrite
	if err := db.Where("owner_kind=? AND owner_id=?", CatalogPhysicalReorganization, id).First(&proof).Error; err != nil || proof.State != state {
		t.Fatalf("physical state %+v expected %s: %v", proof, state, err)
	}
}
