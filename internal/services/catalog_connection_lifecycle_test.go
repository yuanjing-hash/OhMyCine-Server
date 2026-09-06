package services

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloud "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/pan115"
	"gorm.io/gorm"
)

func catalogConnectionFixture(t *testing.T, versioned bool) (*CatalogSnapshotStore, *ConnectionService, Actor, models.MediaLibrary, ConnectionSummary) {
	t.Helper()
	s, library, rec, entries := catalogFixture(t)
	if versioned {
		catalogConvert(t, s, library, rec, entries)
	} else if err := s.writeDB.Where("library_id=?", library.ID).Delete(&models.CatalogHead{}).Error; err != nil {
		t.Fatal(err)
	}
	credentials, err := credential.Open(filepath.Join(t.TempDir(), "test.key"), "")
	if err != nil {
		t.Fatal(err)
	}
	registry := cloud.NewRegistry()
	if err := registry.Register(cloud.ProviderPan115, pan115.New); err != nil {
		t.Fatal(err)
	}
	user := models.User{Username: "catalog-connection", UsernameNormalized: "catalog-connection", PasswordHash: "test", Status: models.UserStatusActive}
	if err := s.writeDB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	actor := Actor{User: user, Permissions: map[string]struct{}{authz.PermissionConnectionsCreate: {}, authz.PermissionConnectionsUpdate: {}}}
	service := NewConnectionService(s.writeDB, NewAuditService(s.writeDB), credentials, registry, zerolog.Nop())
	service.SetCatalogSnapshotStore(s)
	service.SetMediaChangeService(NewMediaChangeService(s.writeDB))
	connection, err := service.Create(actor, ConnectionInput{Name: "catalog-source", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.writeDB.Model(&models.Storage{}).Where("id=?", library.StorageID).Update("connection_id", connection.ID).Error; err != nil {
		t.Fatal(err)
	}
	return s, service, actor, library, connection
}

func TestCatalogConnectionActualCredentialResetAndSameValuePreservation(t *testing.T) {
	s, service, actor, library, connection := catalogConnectionFixture(t, true)
	var before models.CatalogHead
	if err := s.writeDB.First(&before, "library_id=?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	cookie, name := testPan115Cookie, "renamed"
	updated, err := service.Update(actor, connection.ID, UpdateConnectionInput{Cookie: &cookie, Name: &name, Revision: connection.Revision}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var preserved models.CatalogHead
	if err := s.writeDB.First(&preserved, "library_id=?", library.ID).Error; err != nil || !catalogSameHead(before, preserved) {
		t.Fatalf("same-value credentials reset source: %+v %v", preserved, err)
	}
	cookie = strings.Replace(testPan115Cookie, "100_A1", "200_A1", 1)
	if _, err := service.Update(actor, connection.ID, UpdateConnectionInput{Cookie: &cookie, Revision: updated.Revision}, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	var head models.CatalogHead
	var storage models.Storage
	if err := s.writeDB.First(&head, "library_id=?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.writeDB.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	if head.SourceEpoch != before.SourceEpoch+1 || storage.CatalogConnectionEpoch != 1 || storage.CatalogConnectionRevision != 1 || len(catalogReadEntries(t, s, library.ID, "")) != 0 {
		t.Fatalf("old account catalog remained readable: head=%+v epochs=%d/%d", head, storage.CatalogConnectionEpoch, storage.CatalogConnectionRevision)
	}
	var anchorCount, removals int64
	if err := s.writeDB.Model(&models.MediaLibraryEntry{}).Where("library_id=?", library.ID).Count(&anchorCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.writeDB.Model(&models.MediaLibraryChange{}).Where("library_id=? AND kind=? AND state=?", library.ID, models.MediaLibraryChangeRemoval, models.MediaLibraryChangeReady).Count(&removals).Error; err != nil || anchorCount != 2 || removals != 1 {
		t.Fatalf("anchors/removal=%d/%d err=%v", anchorCount, removals, err)
	}
}

func TestCatalogConnectionLegacySourceChangeRefusesButEnabledFencesScan(t *testing.T) {
	s, service, actor, library, connection := catalogConnectionFixture(t, false)
	storage, profile := catalogSourceContext(t, s, library)
	before := mediaLibraryScanSourceFingerprint(library, storage, profile)
	cookie := strings.Replace(testPan115Cookie, "100_A1", "200_A1", 1)
	if _, err := service.Update(actor, connection.ID, UpdateConnectionInput{Cookie: &cookie, Revision: connection.Revision}, RequestContext{}); !errors.Is(err, ErrCatalogFence) || ErrorCode(err) != CodeConflict {
		t.Fatalf("legacy account switch allowed: %v", err)
	}
	enabled := false
	if _, err := service.Update(actor, connection.ID, UpdateConnectionInput{Enabled: &enabled, Revision: connection.Revision}, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	storage, _ = catalogSourceContext(t, s, library)
	if storage.CatalogConnectionEpoch != 0 || storage.CatalogConnectionRevision != 1 || mediaLibraryScanSourceFingerprint(library, storage, profile) == before {
		t.Fatal("enabled state failed to fence old legacy scan")
	}
}

func TestCatalogConnectionTotalLibraryBudgetAndConvertingRefuseAtomically(t *testing.T) {
	s, service, actor, library, connection := catalogConnectionFixture(t, true)
	for index := 0; index < 4; index++ {
		copy := models.MediaLibrary{Name: string(rune('a' + index)), NameNormalized: string(rune('a' + index)), StorageID: library.StorageID, ProfileID: library.ProfileID}
		if err := s.writeDB.Create(&copy).Error; err != nil {
			t.Fatal(err)
		}
	}
	enabled := false
	if _, err := service.Update(actor, connection.ID, UpdateConnectionInput{Enabled: &enabled, Revision: connection.Revision}, RequestContext{}); !errors.Is(err, ErrCatalogBudget) {
		t.Fatalf("unbounded connection fanout allowed: %v", err)
	}
	var current models.Connection
	if err := s.writeDB.First(&current, connection.ID).Error; err != nil || !current.Enabled || current.Revision != connection.Revision {
		t.Fatalf("budget failure partially saved connection: revision=%d err=%v", current.Revision, err)
	}
	if err := s.writeDB.Where("id<>? AND storage_id=?", library.ID, library.StorageID).Delete(&models.MediaLibrary{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.writeDB.Model(&models.CatalogHead{}).Where("library_id=?", library.ID).Update("mode", "converting").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.Update(actor, connection.ID, UpdateConnectionInput{Enabled: &enabled, Revision: connection.Revision}, RequestContext{}); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("connection changed during conversion: %v", err)
	}
}

func TestCatalogLibraryDetailUsesEffectiveCountAndPropagatesReadErrors(t *testing.T) {
	s, library, rec, entries := catalogFixture(t)
	catalogConvert(t, s, library, rec, entries)
	candidate, token := catalogCandidate(t, s, library, "delta", 1)
	deleted := CatalogEntryFromLegacy(entries[0], &rec)
	deleted.Tombstone = true
	if err := s.AppendBatch(context.Background(), candidate.ID, token, CatalogFactBatch{Entries: []models.CatalogEntryFact{deleted}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, s, candidate, token)
	service := NewMediaLibraryService(s.writeDB, NewAuditService(s.writeDB), zerolog.Nop())
	service.SetCatalogSnapshotStore(s)
	detail, err := service.detail(library)
	if err != nil || detail.EntryCount != 1 {
		t.Fatalf("detail counted retained anchors: %d %v", detail.EntryCount, err)
	}
	if err := s.readDB.Callback().Query().Before("gorm:query").Register("test:catalog-detail-read-fault", func(tx *gorm.DB) { _ = tx.AddError(errors.New("injected catalog read failure")) }); err != nil {
		t.Fatal(err)
	}
	if _, err := service.detail(library); err == nil {
		t.Fatal("failed detail returned fabricated count")
	}
}

func TestCatalogLegacyWriterRechecksModeInsideCommit(t *testing.T) {
	s, library, rec, entries := catalogFixture(t)
	for _, mode := range []string{"converting", "versioned"} {
		if mode == "versioned" {
			catalogConvert(t, s, library, rec, entries)
		}
		err := s.writeDB.Transaction(func(tx *gorm.DB) error {
			if err := requireLegacyCatalogWriteTx(tx, library.ID); err != nil {
				return err
			}
			return tx.Model(&models.MediaLibraryEntry{}).Where("id=?", entries[0].ID).Update("title", "forbidden late legacy writer").Error
		})
		if !errors.Is(err, ErrCatalogFence) {
			t.Fatalf("%s accepted late legacy writer: %v", mode, err)
		}
	}
	var retained models.MediaLibraryEntry
	if err := s.writeDB.First(&retained, entries[0].ID).Error; err != nil || retained.Title != entries[0].Title {
		t.Fatal("late legacy commit mutated retained anchor")
	}
}

func TestCatalogConfigProfilePropagationSkipsFrozenConversionButContinuesOtherLibraries(t *testing.T) {
	s, input, library, _, _ := catalogConversionFixture(t)
	candidate, token, err := s.startCatalogConversion(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.captureCatalogConversionInput(context.Background(), candidate, token, input); err != nil {
		t.Fatal(err)
	}
	other := models.MediaLibrary{Name: "after-converting", NameNormalized: "after-converting", ProfileID: library.ProfileID, StorageID: library.StorageID}
	if err := s.writeDB.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.writeDB.Model(&models.MediaClassificationProfile{}).Where("id=?", library.ProfileID).Update("revision", gorm.Expr("revision+1")).Error; err != nil {
		t.Fatal(err)
	}
	service := NewMediaLibraryService(s.writeDB, NewAuditService(s.writeDB), zerolog.Nop())
	service.SetCatalogSnapshotStore(s)
	if err := service.ProfileRevisionChanged(library.ProfileID, library.ProfileRevision+1); err != nil {
		t.Fatalf("frozen conversion broke committed Profile propagation: %v", err)
	}
	if err := s.writeDB.First(&other, other.ID).Error; err != nil || !other.ReclassificationDue {
		t.Fatalf("later library was skipped: %+v %v", other, err)
	}
	if err := s.captureCatalogConversionInput(context.Background(), candidate, token, input); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("old conversion laundered new Profile: %v", err)
	}
}
