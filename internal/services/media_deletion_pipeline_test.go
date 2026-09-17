package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

func TestProviderDeletionWholePipelineCleansExactSTRMAndReplays(t *testing.T) {
	management, queue, _, library, root := strmManagementFixture(t)
	s := management.libraries
	db := s.db
	ctx := context.Background()
	var storage models.Storage
	db.First(&storage, library.StorageID)
	var profile models.MediaClassificationProfile
	db.First(&profile, library.ProfileID)
	library.DirtyGeneration = 2
	library.ProviderRootID = "root"
	library.MetadataArtifactsEnabled = false
	if err := db.Save(&library).Error; err != nil {
		t.Fatal(err)
	}
	artifacts := NewMediaArtifactService(db, queue, &SignedProxyService{}, zerolog.Nop())
	artifacts.SetCleanupService(management)
	s.SetArtifactService(artifacts)
	s.backends.Register(pan115MediaLibraryBackend{driver: func(uint) (cloudpkg.Driver, error) { t.Fatal("deletion called provider"); return nil, nil }})
	before := time.Now().UTC().Add(-time.Hour)
	rec := models.MediaLibraryRecognition{LibraryID: library.ID, SourceKey: "same-show", InputFingerprint: "same-show", ProfileID: profile.ID, ProfileRevision: profile.Revision, Status: mediaRecognitionStatusMatched, Title: "One work", MediaType: "movie", LastGeneration: 2, MetadataJSON: "{}", CreatedAt: before, UpdatedAt: before}
	if err := db.Create(&rec).Error; err != nil {
		t.Fatal(err)
	}
	entries := []models.MediaLibraryEntry{{LibraryID: library.ID, ProviderID: "deleted-version", RelativePath: "/Movie/1080p.mkv", RecognitionID: &rec.ID, LastGeneration: 2, CreatedAt: before, UpdatedAt: before}, {LibraryID: library.ID, ProviderID: "retained-version", RelativePath: "/Movie/2160p.mkv", RecognitionID: &rec.ID, LastGeneration: 2, CreatedAt: before, UpdatedAt: before}}
	if err := db.Create(&entries).Error; err != nil {
		t.Fatal(err)
	}
	old := library
	old.DirtyGeneration = 1
	scan := models.MediaLibraryScanRun{LibraryID: library.ID, Generation: 2, Kind: "full", Status: "success", SourceFingerprint: mediaLibraryScanSourceFingerprint(old, storage, profile), CheckpointJSON: "{}", StartedAt: before}
	if err := db.Create(&scan).Error; err != nil {
		t.Fatal(err)
	}
	_, rootIdentity, err := canonicalProjectionRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	policy, _ := json.Marshal(mediaArtifactPolicy{LibraryID: library.ID, StorageID: storage.ID, ScanRunID: scan.ID, Generation: 2, ProjectionRoot: root, ProjectionRootIdentity: rootIdentity, TargetKind: models.MediaArtifactTargetLocalProjection, STRMEnabled: true})
	owner := models.MediaArtifactRun{ID: uuid.NewString(), LibraryID: library.ID, Generation: 2, Status: models.MediaArtifactStatusCompleted, PolicyJSON: string(policy)}
	if err := db.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}
	for i, e := range entries {
		relative := []string{"/Movie/1080p.strm", "/Movie/2160p.strm"}[i]
		full := filepath.Join(root, filepath.FromSlash(relative[1:]))
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		content := []byte("synthetic-signed-projection\n")
		if err := os.WriteFile(full, content, 0600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(content)
		row := models.MediaArtifact{OpaqueID: uuid.NewString(), RunID: owner.ID, LibraryID: library.ID, SourceIdentity: fmt.Sprintf("entry:%d", e.ID), ProviderItemID: e.ProviderID, Kind: models.MediaArtifactKindSTRM, TargetKind: models.MediaArtifactTargetLocalProjection, RelativePath: relative, Managed: true, Active: true, Status: models.MediaArtifactStatusCompleted, ContentFingerprint: hex.EncodeToString(digest[:])}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	poster := filepath.Join(root, "Movie", "poster.jpg")
	if err := os.WriteFile(poster, []byte("shared-unmanaged-poster"), 0600); err != nil {
		t.Fatal(err)
	}
	payload := providerEventPayload{Kind: cloudpkg.ChangeDeleted, ItemID: entries[0].ProviderID, ParentID: "0", Name: "1080p.mkv"}
	raw, _ := json.Marshal(payload)
	inbox := models.ProviderEvent{ConnectionID: *storage.ConnectionID, Stream: "life", ProviderEventID: "800", EventTime: time.Now().UTC().Add(time.Second), Kind: payload.Kind, ItemID: payload.ItemID, PayloadJSON: string(raw)}
	if err := db.Create(&inbox).Error; err != nil {
		t.Fatal(err)
	}
	delivery := models.MediaLibraryProviderEvent{LibraryID: library.ID, InboxEventID: inbox.ID, PayloadJSON: string(raw), SourceFingerprint: catalogSourceFingerprint(library, storage)}
	if err := db.Create(&delivery).Error; err != nil {
		t.Fatal(err)
	}
	prepared, err := s.prepareProviderDeliveryPage(ctx, library.ID, providerChangeScope{DeliveryIDs: []uint{delivery.ID}})
	if err != nil {
		t.Fatal(err)
	}
	eventCtx := withProviderChangeScope(ctx, prepared)
	if _, err := s.reconcile(eventCtx, library.ID, "event"); err != nil {
		t.Fatal(err)
	}
	claim, err := queue.Claim([]string{JobTypeMediaArtifact})
	if err != nil || claim == nil {
		t.Fatalf("missing real artifact job: %v", err)
	}
	result := NewMediaArtifactWorker(artifacts).Run(ctx, workerRuntime{queue: queue, job: *claim}, *claim)
	if result.ErrorCode != "" || result.RetryAt != nil {
		t.Fatalf("worker=%+v", result)
	}
	if err := queue.Complete(claim.Job.ID, claim.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "Movie", "1080p.strm")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted STRM remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "Movie", "2160p.strm")); err != nil {
		t.Fatal("other version removed", err)
	}
	if b, err := os.ReadFile(poster); err != nil || string(b) != "shared-unmanaged-poster" {
		t.Fatal("shared poster changed")
	}
	var remaining int64
	db.Model(&models.MediaLibraryEntry{}).Where("library_id = ?", library.ID).Count(&remaining)
	if remaining != 1 {
		t.Fatalf("versions remaining=%d", remaining)
	}
	if err := s.ackProviderChangeScope(ctx, library.ID, prepared.DeliveryIDs); err != nil {
		t.Fatal(err)
	}
	var jobsBefore, jobsAfter int64
	db.Model(&models.Job{}).Where("job_type = ?", JobTypeMediaArtifact).Count(&jobsBefore)
	// Replay the frozen event after acknowledgement loss; no catalog/physical work remains.
	again, err := s.prepareProviderDeliveryPage(ctx, library.ID, providerChangeScope{DeliveryIDs: []uint{delivery.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if !again.empty() {
		if _, err := s.reconcile(withProviderChangeScope(ctx, again), library.ID, "event"); err != nil {
			t.Fatal(err)
		}
	}
	db.Model(&models.Job{}).Where("job_type = ?", JobTypeMediaArtifact).Count(&jobsAfter)
	if jobsBefore != jobsAfter {
		t.Fatal("replay allocated another artifact job")
	}
}
