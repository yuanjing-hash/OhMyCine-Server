package database

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestCatalogCollectionsMigrationV79IsEmptyAdditiveAndRepeatable(t *testing.T) {
	db := structureMigrationDB(t, 78)
	collectionID := int64(9001)
	legacy := models.PlayerMediaCollection{ID: uuid.NewString(), Source: "tmdb", Kind: "collection", Name: "Keep old metadata", TMDBCollectionID: &collectionID, Visible: true, Revision: 8}
	if err := db.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	for _, model := range []any{&models.CatalogCollectionIdentity{}, &models.CatalogCollectionMemberFact{}, &models.CatalogCollectionPreparation{}} {
		var count int64
		if err := db.Model(model).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("startup projected collections: %T count=%d err=%v", model, count, err)
		}
		if raw, err := json.Marshal(model); err != nil || string(raw) != "{}" {
			t.Fatalf("private model leaked: %s %v", raw, err)
		}
	}
	var after models.PlayerMediaCollection
	if err := db.First(&after, "id=?", legacy.ID).Error; err != nil || after.Name != legacy.Name || after.Revision != legacy.Revision || !after.Visible {
		t.Fatalf("old collection changed: %+v %v", after, err)
	}
}
