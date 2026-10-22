//go:build quark_live

package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	storagefs "github.com/yuanjing-hash/OhMyCine-Server/internal/storage"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"gorm.io/gorm"
)

// QuarkObservationCreationIdentity reads the original, immutable first positive
// snapshot of an isolated acceptance database. It is not a current catalog read
// or a cloud identity conversion. A missing/collected initial snapshot fails
// closed; later rename/move observations cannot replace creation evidence.
func QuarkObservationCreationIdentity(ctx context.Context, stateDir, account, rootID, rootPath, name string, size int64) (cloud.Item, error) {
	var out cloud.Item
	invalid := errors.New("creation_identity_unavailable")
	if !filepath.IsAbs(stateDir) || account == "" || !quark.ObservationTVIdentity(rootID, account) || name == "" || filepath.Base(name) != name || size <= 0 {
		return out, invalid
	}
	dbPath := filepath.Join(stateDir, "observation.db")
	for p := dbPath; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil || storagefs.IsReparsePoint(p, info) || p == dbPath && !info.Mode().IsRegular() || p != dbPath && !info.IsDir() {
			return out, invalid
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	lock, err := database.AcquireExclusiveRuntime(dbPath)
	if err != nil {
		return out, invalid
	}
	defer lock.Close()
	db, err := database.OpenReadOnly(dbPath)
	if err != nil {
		return out, invalid
	}
	pool, err := db.DB()
	if err != nil {
		return out, invalid
	}
	defer pool.Close()
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var libraries []models.MediaLibrary
		if tx.Limit(2).Find(&libraries).Error != nil || len(libraries) != 1 {
			return invalid
		}
		lib := libraries[0]
		if lib.Name != "isolated-observation" || lib.ProviderRootID != rootID || lib.RelativeRoot != "/" || !lib.Enabled {
			return invalid
		}
		var storage models.Storage
		if tx.First(&storage, lib.StorageID).Error != nil || storage.Type != models.StorageTypeQuark || storage.ConnectionID == nil || storage.RootPath != rootID || storage.RootDisplayPath != rootPath || !storage.Enabled {
			return invalid
		}
		var connection models.Connection
		if tx.First(&connection, *storage.ConnectionID).Error != nil || connection.Provider != models.ConnectionProviderQuark || connection.AccountID != account || !connection.Enabled {
			return invalid
		}
		var scopes []models.ProviderObservationScope
		if tx.Where("library_id = ?", lib.ID).Limit(2).Find(&scopes).Error != nil || len(scopes) != 1 || scopes[0].RootID != rootID || scopes[0].ConnectionID != connection.ID || !scopes[0].Enabled || scopes[0].LastCompleteAt == nil {
			return invalid
		}
		expectedScope, err := quarkObservationScopeTx(tx, lib.ID)
		if err != nil || scopes[0].Provider != expectedScope.Provider || scopes[0].Kind != expectedScope.Kind || scopes[0].RootPath != expectedScope.RootPath || scopes[0].AccountKey != expectedScope.AccountKey || scopes[0].Fence != expectedScope.Fence {
			return invalid
		}
		var head models.CatalogHead
		if tx.First(&head, "library_id = ?", lib.ID).Error != nil || head.Mode != "versioned" || head.SourceEpoch == 0 {
			return invalid
		}
		var profile models.MediaClassificationProfile
		if tx.First(&profile, lib.ProfileID).Error != nil || head.SourceFingerprint != catalogSourceFingerprint(lib, storage) || head.ConfigFingerprint != catalogConfigFingerprint(lib, storage, profile) {
			return invalid
		}
		// Revision 1 is the deliberately empty initial head. The first positive
		// publication is revision 2; never substitute a newer snapshot or live row.
		var initial []models.CatalogSnapshot
		if tx.Where("library_id = ? AND state = ? AND published_revision = ? AND source_epoch = ?", lib.ID, "published", 1, head.SourceEpoch).Limit(2).Find(&initial).Error != nil || len(initial) != 1 || initial[0].Kind != "base" || initial[0].SourceFingerprint != head.SourceFingerprint || initial[0].ConfigFingerprint != head.ConfigFingerprint {
			return invalid
		}
		var initialFacts int64
		if tx.Model(&models.CatalogEntryFact{}).Where("snapshot_id = ?", initial[0].ID).Count(&initialFacts).Error != nil || initialFacts != 0 {
			return invalid
		}
		var snapshots []models.CatalogSnapshot
		if tx.Where("library_id = ? AND state = ? AND published_revision = ? AND source_epoch = ?", lib.ID, "published", 2, head.SourceEpoch).Limit(2).Find(&snapshots).Error != nil || len(snapshots) != 1 {
			return invalid
		}
		snapshot := snapshots[0]
		if snapshot.SourceFingerprint != head.SourceFingerprint || snapshot.ConfigFingerprint != head.ConfigFingerprint {
			return invalid
		}
		var facts []models.CatalogEntryFact
		if tx.Where("snapshot_id = ?", snapshot.ID).Limit(2).Find(&facts).Error != nil || len(facts) != 1 {
			return invalid
		}
		fact := facts[0]
		if fact.Tombstone || fact.LibraryID != lib.ID || fact.RelativePath != "/"+name || fact.Size != size || !quark.ObservationTVIdentity(fact.ProviderID, account) {
			return invalid
		}
		out = cloud.Item{ID: fact.ProviderID, ParentID: rootID, Name: name, Size: size}
		return nil
	})
	if err != nil {
		return cloud.Item{}, invalid
	}
	return out, nil
}
