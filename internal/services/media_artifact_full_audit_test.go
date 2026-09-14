package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLegacyFullArtifactWorkerAuditsRepairsAndDeletes(t *testing.T) {
	management, queue, _, library, root := strmManagementFixture(t)
	db := management.db
	if err := db.Model(&models.Storage{}).Where("id = ?", library.StorageID).Updates(map[string]any{"type": models.StorageTypeLocal, "root_path": root}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&library).Updates(map[string]any{"strm_enabled": false, "signed_proxy_enabled": false}).Error; err != nil {
		t.Fatal(err)
	}
	metadata, err := marshalRecognitionMetadata(MediaRecognitionResult{Snapshot: tmdb.Snapshot{Version: 1, TMDBID: 42, MediaType: "movie", Title: "Movie"}})
	if err != nil {
		t.Fatal(err)
	}
	rec := models.MediaLibraryRecognition{LibraryID: library.ID, SourceKey: "movie", ProfileID: library.ProfileID, ProfileRevision: library.ProfileRevision, Status: mediaRecognitionStatusMatched, MediaType: "movie", Title: "Movie", MetadataJSON: metadata, LastGeneration: 3}
	if err := db.Create(&rec).Error; err != nil {
		t.Fatal(err)
	}
	entry := models.MediaLibraryEntry{LibraryID: library.ID, RelativePath: "/Movie.mkv", RecognitionID: &rec.ID, MediaType: "movie", Title: "Movie", MatchStatus: "matched", LastGeneration: 3}
	if err := db.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Movie.mkv"), []byte("video"), 0600); err != nil {
		t.Fatal(err)
	}
	service := NewMediaArtifactService(db, queue, nil, zerolog.Nop())
	service.SetCleanupService(management)
	execute := func(generation uint64) models.MediaArtifactRun {
		t.Helper()
		now := time.Now().UTC()
		if err := db.Model(&rec).Update("last_generation", generation).Error; err != nil {
			t.Fatal(err)
		}
		scan := models.MediaLibraryScanRun{LibraryID: library.ID, Generation: generation, Kind: "full", Status: "success", StartedAt: now, FinishedAt: &now}
		if err := db.Create(&scan).Error; err != nil {
			t.Fatal(err)
		}
		if err := service.ScheduleGeneration(library.ID, generation); err != nil {
			t.Fatal(err)
		}
		claim, err := queue.Claim([]string{JobTypeMediaArtifact})
		if err != nil || claim == nil {
			t.Fatalf("claim=%v err=%v", claim, err)
		}
		result := NewMediaArtifactWorker(service).Run(context.Background(), &providerWakeRuntime{}, *claim)
		if result.ErrorCode != "" {
			t.Fatalf("generation %d: %+v", generation, result)
		}
		if err := queue.Complete(claim.Job.ID, claim.LeaseToken); err != nil {
			t.Fatal(err)
		}
		var run models.MediaArtifactRun
		if err := db.Where("library_id = ? AND generation = ?", library.ID, generation).First(&run).Error; err != nil {
			t.Fatal(err)
		}
		return run
	}
	first := execute(3)
	if first.WrittenCount != 1 {
		t.Fatalf("first=%+v", first)
	}
	target := filepath.Join(root, "Movie.nfo")
	original, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(target)
	healthy := execute(4)
	after, _ := os.Stat(target)
	if healthy.ExpectedCount != 0 || healthy.WrittenCount != 0 || healthy.UpdatedCount != 0 || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("healthy rewrote: %+v", healthy)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	repaired := execute(5)
	if repaired.ExpectedCount != 1 {
		t.Fatalf("missing=%+v", repaired)
	}
	if err := os.WriteFile(target, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	repaired = execute(6)
	body, err := os.ReadFile(target)
	if err != nil || string(body) != string(original) || repaired.UpdatedCount != 1 {
		t.Fatalf("corrupt=%+v body=%q err=%v", repaired, body, err)
	}
	if err := db.Delete(&entry).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(&rec).Error; err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "Movie.mkv")); err != nil {
		t.Fatal(err)
	}
	execute(7)
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("deleted source projection remains: %v", err)
	}
}

func TestLegacyFullArtifactAuditSidecarSourceFingerprint(t *testing.T) {
	root := t.TempDir()
	content := []byte("subtitle")
	target := filepath.Join(root, "video.srt")
	if err := os.WriteFile(target, content, 0600); err != nil {
		t.Fatal(err)
	}
	asset := models.MediaLibrarySourceAsset{ID: 9, ProviderID: "cloud", RelativePath: "/video.srt", Size: int64(len(content)), ModifiedAt: time.Now().UTC(), HashHint: "original"}
	digest := sha256.Sum256(content)
	row := models.MediaArtifact{ID: 3, RunID: "old", SourceIdentity: fmt.Sprintf("asset:%d", asset.ID), RelativePath: asset.RelativePath, TargetKind: models.MediaArtifactTargetLocalProjection, Managed: true, Active: true, Status: models.MediaArtifactStatusCompleted, ContentFingerprint: hex.EncodeToString(digest[:]), SourceFingerprint: artifactAssetSourceFingerprint(asset)}
	audit := func(assets []models.MediaLibrarySourceAsset) (int, *artifactManifestIndex) {
		m := newArtifactManifestIndex(1)
		m.rows[artifactManifestKey(row.TargetKind, row.RelativePath)] = row
		_, _, pending := (&MediaArtifactService{}).filterLegacyFullArtifactWork(root, models.MediaArtifactRun{ID: "next"}, mediaArtifactPolicy{}, nil, nil, assets, nil, m, signedArtifactVerifier{})
		return len(pending), m
	}
	if pending, m := audit([]models.MediaLibrarySourceAsset{asset}); pending != 0 || len(m.dirty) != 1 {
		t.Fatal("healthy not retained")
	}
	changed := asset
	changed.HashHint = "new"
	if pending, _ := audit([]models.MediaLibrarySourceAsset{changed}); pending != 1 {
		t.Fatal("source hash change ignored")
	}
	if pending, m := audit(nil); pending != 0 || len(m.dirty) != 0 {
		t.Fatal("deleted source retained")
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if pending, _ := audit([]models.MediaLibrarySourceAsset{asset}); pending != 1 {
		t.Fatal("missing sidecar ignored")
	}
}
