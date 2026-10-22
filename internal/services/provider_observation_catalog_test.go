package services

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
)

func TestProviderObservationRealCatalogPublicationPreservesIdentity(t *testing.T) {
	s, library, _ := createCatalogTestLibrary(t)
	var storage models.Storage
	var profile models.MediaClassificationProfile
	if err := s.db.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.First(&profile, library.ProfileID).Error; err != nil {
		t.Fatal(err)
	}
	storage.Type = models.StorageTypeQuark
	connection := models.Connection{Name: "observed-quark", NameNormalized: "observed-quark", Provider: models.ConnectionProviderQuark, Enabled: true}
	if err := s.db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	storage.ConnectionID = &connection.ID
	library.ProviderRootID = "root"
	library.Enabled = true
	library.STRMEnabled, library.SignedProxyEnabled = true, true
	library.MetadataArtifactsEnabled = false
	library.STRMLocalRoot = t.TempDir()
	if err := s.db.Save(&storage).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Save(&library).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error { return initializeFreshQuarkCatalogTx(tx, library, storage, profile) }); err != nil {
		t.Fatal(err)
	}
	var location struct{ File string }
	if err := s.db.Raw("PRAGMA database_list").Scan(&location).Error; err != nil {
		t.Fatal(err)
	}
	readDB, err := database.OpenReadOnly(location.File)
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := readDB.DB()
	t.Cleanup(func() { _ = pool.Close() })
	s.SetCatalogSnapshotStore(NewCatalogSnapshotStore(s.db, readDB))
	s.changes = NewMediaChangeService(s.db)
	s.queue = NewQueueService(s.db, NewAuditService(s.db))
	secrets, err := credential.Open(filepath.Join(t.TempDir(), "credential.key"), "")
	if err != nil {
		t.Fatal(err)
	}
	connections := NewConnectionService(s.db, s.audit, secrets, cloud.NewRegistry(), zerolog.Nop())
	proxy, err := NewSignedProxyService(s.db, secrets, connections, "https://media.example.test", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	artifacts := NewMediaArtifactService(s.db, s.queue, proxy, zerolog.Nop())
	artifacts.SetCatalogSnapshotStore(s.catalogStore)
	artifacts.SetMediaChangeService(s.changes)
	artifacts.SetCleanupService(NewSTRMManagementService(s.db, s.audit, s.queue, s, artifacts))
	s.SetArtifactService(artifacts)
	ready := 0
	s.changes.SetReadyHandler(func(uint, uint64) { ready++ })
	drainArtifacts := func() {
		t.Helper()
		if err := s.RecoverCatalogScanFollowups(context.Background(), 10); err != nil {
			t.Fatal(err)
		}
		for range 10 {
			job, err := s.queue.Claim([]string{JobTypeMediaArtifact})
			if err != nil {
				t.Fatal(err)
			}
			if job == nil {
				for range 10 {
					more, err := s.changes.DispatchBatch(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					if !more {
						return
					}
				}
				t.Fatal("media ready dispatch did not converge")
				return
			}
			if result := NewMediaArtifactWorker(artifacts).Run(context.Background(), &providerWakeRuntime{}, *job); result.ErrorCode != "" {
				t.Fatalf("artifact worker: %+v", result)
			}
			if err := s.queue.Complete(job.Job.ID, job.LeaseToken); err != nil {
				t.Fatal(err)
			}
		}
		t.Fatal("artifact queue did not converge")
	}
	assertSTRM := func(name string) []byte {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(library.STRMLocalRoot, name))
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(strings.TrimSpace(string(body)))
		if err != nil || u.Host != "media.example.test" || u.Query().Get("sig") == "" || u.Query().Get("exp") == "" {
			t.Fatalf("not a signed Server URL: %q %v", body, err)
		}
		return body
	}
	s.EnableCatalogScanFollowups()
	clock := time.Now().UTC()
	observer := NewProviderObservationService(s.db)
	observer.now = func() time.Time { return clock }
	observer.SetFenceValidator(func(*gorm.DB, models.ProviderObservationScope) error { return nil })
	observer.SetPublisher(s)
	scope, err := observer.EnsureScope(context.Background(), ObservationScopeInput{LibraryID: library.ID, ConnectionID: 1, AccountKey: "account", Provider: "quark", Kind: "library", RootID: "root", RootPath: "/", Fence: "fence"})
	if err != nil {
		t.Fatal(err)
	}
	reader := &observationTestReader{tree: map[string][]ObservationFact{"root": {obsFile("stable-original", "root", "Movie.2024.mkv")}}}
	f := observationFixture{db: s.db, s: observer, scope: scope, clock: &clock, reader: reader}
	f.round(t)
	f.publish(t)
	drainArtifacts()
	firstSTRM := assertSTRM("Movie.2024.strm")
	firstReady := ready
	if firstReady == 0 {
		t.Fatal("completed STRM did not publish media ready")
	}
	entries := catalogReadEntries(t, s.catalogStore, library.ID, "")
	if len(entries) != 1 {
		t.Fatalf("initial entries=%+v", entries)
	}
	original := entries[0]
	if original.ProviderID != "stable-original" {
		t.Fatalf("provider identity=%+v", original)
	}
	var followups int64
	if err := s.db.Model(&models.CatalogScanFollowup{}).Where("library_id = ?", library.ID).Count(&followups).Error; err != nil || followups == 0 {
		t.Fatalf("missing atomic followup %d %v", followups, err)
	}
	// The same observation contains no catalog change and no new head revision.
	var before models.CatalogHead
	if err := s.db.First(&before, "library_id = ?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	f.advance(time.Minute)
	f.round(t)
	f.publish(t)
	drainArtifacts()
	if got := assertSTRM("Movie.2024.strm"); string(got) != string(firstSTRM) || ready != firstReady {
		t.Fatal("no-op observation rewrote STRM or repeated refresh notification")
	}
	var after models.CatalogHead
	if err := s.db.First(&after, "library_id = ?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision {
		t.Fatal("no-op observation changed catalog head")
	}
	reader.tree["root"][0].Name = "Movie.Renamed.2024.mkv"
	f.advance(time.Minute)
	f.round(t)
	f.publish(t)
	drainArtifacts()
	assertSTRM("Movie.Renamed.2024.strm")
	if _, err := os.Stat(filepath.Join(library.STRMLocalRoot, "Movie.2024.strm")); !os.IsNotExist(err) {
		t.Fatalf("old owned STRM survived rename: %v", err)
	}
	entries = catalogReadEntries(t, s.catalogStore, library.ID, "")
	if len(entries) != 1 || entries[0].ID != original.ID || !entries[0].CreatedAt.Equal(original.CreatedAt) {
		t.Fatalf("rename identity/time changed: %+v original=%+v", entries, original)
	}
	reader.tree["root"] = nil
	f.advance(11 * time.Minute)
	f.round(t)
	f.publish(t)
	drainArtifacts()
	if rows := catalogReadEntries(t, s.catalogStore, library.ID, ""); len(rows) != 1 {
		t.Fatal("first missing downlisted media")
	}
	f.advance(11 * time.Minute)
	f.round(t)
	f.publish(t)
	drainArtifacts()
	if rows := catalogReadEntries(t, s.catalogStore, library.ID, ""); len(rows) != 0 {
		t.Fatalf("confirmed absence not removed: %+v", rows)
	}
	if _, err := os.Stat(filepath.Join(library.STRMLocalRoot, "Movie.Renamed.2024.strm")); !os.IsNotExist(err) {
		t.Fatalf("confirmed removed STRM retained: %v", err)
	}
	reader.tree["root"] = []ObservationFact{obsFile("stable-original", "root", "Movie.Restored.2024.mkv")}
	f.advance(time.Minute)
	f.round(t)
	f.publish(t)
	drainArtifacts()
	assertSTRM("Movie.Restored.2024.strm")
	entries = catalogReadEntries(t, s.catalogStore, library.ID, "")
	if len(entries) != 1 || entries[0].ID != original.ID {
		t.Fatalf("restore lost stable identity: %+v original=%+v", entries, original)
	}
	if !entries[0].CreatedAt.Equal(original.CreatedAt) {
		t.Fatalf("restore changed ingestion time: %v != %v", entries[0].CreatedAt, original.CreatedAt)
	}
}
