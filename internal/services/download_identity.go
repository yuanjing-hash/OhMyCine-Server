package services

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"gorm.io/gorm"
)

var (
	errDownloadIdentityInvalid = errors.New("download identity binding is inconsistent")
	errDownloadIdentityChanged = errors.New("download identity changed during verification")
)

func invalidDownloadIdentity() error {
	return appError(CodeTransferMediaUnrecognized, "媒体识别结果不可信，未创建自动入库任务", errDownloadIdentityInvalid)
}

// The versioned identity owns provenance and lock semantics. Legacy override
// columns are only an exact-ID/routing projection, not proof of a user action.
func downloadCanonicalIdentityBinding(task models.DownloadTask) (*DownloadRecognitionIdentity, error) {
	raw := strings.TrimSpace(task.IdentitySnapshotJSON)
	if raw == "" || raw == "{}" {
		return nil, nil // Existing pre-snapshot automatic/manual recovery below.
	}
	snapshot, err := decodeMediaIdentity(raw)
	if err != nil || task.IdentityRevision == 0 || snapshot.Revision != task.IdentityRevision || snapshot.Source != task.IdentitySource || snapshot.Status != task.IdentityStatus || snapshot.Locked != task.IdentityLocked {
		return nil, invalidDownloadIdentity()
	}
	if snapshot.Source == mediaIdentitySourceLocalProvisional && snapshot.Status == mediaIdentityStatusLocalProvisional && !snapshot.Locked && snapshot.TMDBID == nil {
		return nil, nil
	}
	if snapshot.TMDBID == nil || *snapshot.TMDBID <= 0 || (snapshot.MediaType != "tv" && snapshot.MediaType != "movie") {
		return nil, invalidDownloadIdentity()
	}
	// A real manual correction atomically advances the canonical snapshot and
	// exact override first; the old scrape projection is recomputed on this
	// completion pass. It cannot veto that newer authoritative choice.
	if task.ScrapeTMDBID != nil && snapshot.Category != "" && snapshot.Source != mediaIdentitySourceManual {
		if validateTransferIdentitySnapshot(task) != nil || !sameOptional(snapshot.Season, task.ScrapeSeason) || !sameOptional(snapshot.Episode, task.ScrapeEpisode) {
			return nil, invalidDownloadIdentity()
		}
	}
	identity := DownloadRecognitionIdentity{TMDBID: *snapshot.TMDBID, MediaType: snapshot.MediaType, Source: snapshot.Source, Status: snapshot.Status, Locked: snapshot.Locked, Season: cloneInt(snapshot.Season), Episode: cloneInt(snapshot.Episode)}
	if !validDownloadIdentityBinding(identity) {
		return nil, invalidDownloadIdentity()
	}
	if task.RecognitionOverrideTMDBID != nil {
		if *task.RecognitionOverrideTMDBID != identity.TMDBID || task.RecognitionOverrideMediaType != identity.MediaType || (snapshot.Season != nil && task.RecognitionOverrideSeason != nil && !sameOptional(snapshot.Season, task.RecognitionOverrideSeason)) || (snapshot.Episode != nil && task.RecognitionOverrideEpisode != nil && !sameOptional(snapshot.Episode, task.RecognitionOverrideEpisode)) {
			return nil, invalidDownloadIdentity()
		}
		// Old submission snapshots omitted these two fields. Preserve their
		// already-frozen routing projection until the next full snapshot is saved.
		if identity.Season == nil {
			identity.Season = cloneInt(task.RecognitionOverrideSeason)
		}
		if identity.Episode == nil {
			identity.Episode = cloneInt(task.RecognitionOverrideEpisode)
		}
	}
	return &identity, nil
}

// A released Follow handoff populated the legacy override, then completion
// incorrectly set direct_id/verified/locked at r1. Correct only this exact
// source-backed tuple in memory; final GetByID and package checks still run,
// and persistence repeats the original intent check under the writer fence.
func completedDownloadIdentity(db *gorm.DB, task models.DownloadTask, manifest downloadpkg.Manifest) (models.DownloadTask, error) {
	if task.IdentitySource != mediaIdentitySourceDirectID || !task.IdentityLocked {
		return task, nil
	}
	if task.IdentityRevision != 1 || task.IdentityStatus != mediaIdentityStatusVerified || task.FollowSubscriptionID == "" || task.FollowResourceFingerprint == "" || task.RecognitionOverrideTMDBID == nil || task.RecognitionOverrideMediaType != "tv" || task.ScrapeMediaType != "tv" || task.RecognitionOverrideSeason == nil || !sameOptional(task.RecognitionOverrideSeason, task.ScrapeSeason) || !sameOptional(task.RecognitionOverrideEpisode, task.ScrapeEpisode) {
		return task, invalidDownloadIdentity()
	}
	snapshot, err := decodeMediaIdentity(task.IdentitySnapshotJSON)
	if err != nil || !snapshot.Locked || validateTransferIdentityProjection(task, snapshot) != nil || !sameOptional(snapshot.Season, task.ScrapeSeason) || !sameOptional(snapshot.Episode, task.ScrapeEpisode) || !sameOptional(snapshot.Year, task.ScrapeYear) || !sameOptional(snapshot.Confidence, task.ScrapeConfidence) || !sameOptional(snapshot.TMDBID, task.RecognitionOverrideTMDBID) {
		return task, invalidDownloadIdentity()
	}
	stored, exists, err := completedDownloadManifest(task.CompletedManifestJSON)
	if err != nil || !exists || !manifest.Complete || stored.Name != manifest.Name || !sameAutomaticTransferFiles(stored.Files, manifest.Files) {
		return task, invalidDownloadIdentity()
	}
	snapshot.Locked = false
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return task, err
	}
	normalized := task
	normalized.IdentityLocked, normalized.IdentitySnapshotJSON = false, string(raw)
	normalized.ScrapeStatus = "completed_verified"
	selected, err := selectDownloadPackageManifest(manifest, "tv")
	if err != nil || validateAutomaticTransferSnapshot(normalized, selected) != nil {
		return task, invalidDownloadIdentity()
	}
	_, expected, err := buildDownloadIdentitySnapshot(normalized, scrapeMatch{Title: task.ScrapeTitle, TMDBID: task.ScrapeTMDBID, MediaType: task.ScrapeMediaType, Category: task.ScrapeCategory, Year: task.ScrapeYear, Confidence: task.ScrapeConfidence, Season: task.ScrapeSeason, Episode: task.ScrapeEpisode}, selected, task.IdentitySource, task.IdentityStatus, false, task.IdentityRevision)
	if err != nil || expected != normalized.IdentitySnapshotJSON || validateLegacyFollowIntent(db, task, normalized, selected) != nil {
		return task, invalidDownloadIdentity()
	}
	return normalized, nil
}

// Projection validation is shared with the strict Transfer gate by validating
// an otherwise identical unlocked candidate. It never widens that gate.
func validateTransferIdentityProjection(task models.DownloadTask, snapshot MediaIdentitySnapshot) error {
	snapshot.Locked = false
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	task.IdentityLocked, task.IdentitySnapshotJSON = false, string(raw)
	return validateTransferIdentitySnapshot(task)
}

func validateLegacyFollowIntent(db *gorm.DB, original, normalized models.DownloadTask, selected downloadpkg.Manifest) error {
	var subscription models.FollowSubscription
	if err := db.First(&subscription, "id = ? AND owner_id = ?", original.FollowSubscriptionID, original.OwnerID).Error; err != nil {
		return err
	}
	if subscription.MediaType != "tv" || subscription.TMDBID != *original.RecognitionOverrideTMDBID || original.TargetLibraryID == nil {
		return errDownloadIdentityInvalid
	}
	var claims []models.FollowEpisodeClaim
	if err := db.Where("subscription_id = ? AND resource_fingerprint = ? AND (download_task_id = ? OR (download_task_id IS NULL AND state = ?))", original.FollowSubscriptionID, original.FollowResourceFingerprint, original.ID, "failed").Limit(10001).Find(&claims).Error; err != nil {
		return err
	}
	if len(claims) == 0 || len(claims) > 10000 || claims[0].RunID == nil {
		return errDownloadIdentityInvalid
	}
	var run models.FollowRun
	if err := db.First(&run, "id = ? AND subscription_id = ? AND owner_id = ?", *claims[0].RunID, subscription.ID, original.OwnerID).Error; err != nil {
		return err
	}
	var frozen FollowExecutionSnapshot
	if run.SubscriptionRevision == 0 || decodeStrictJSON(run.ExecutionSnapshotJSON, &frozen) != nil || (frozen.Version != 1 && frozen.Version != 2) || frozen.MediaLibraryID != *original.TargetLibraryID {
		return errDownloadIdentityInvalid
	}
	if frozen.Version == 2 && (frozen.RoutingPolicy != "source_priority" || frozen.DownloaderID != "") || frozen.Version == 1 && frozen.RoutingPolicy != "" {
		return errDownloadIdentityInvalid
	}
	season := *original.RecognitionOverrideSeason
	seasonSelected := false
	for _, value := range frozen.Seasons {
		seasonSelected = seasonSelected || value == season
	}
	if !seasonSelected {
		return errDownloadIdentityInvalid
	}
	wanted := make(map[[2]int]struct{}, len(claims))
	for _, claim := range claims {
		if claim.RunID == nil || *claim.RunID != run.ID || claim.SeasonNumber != season || claim.EpisodeNumber <= 0 || original.RecognitionOverrideEpisode != nil && claim.EpisodeNumber != *original.RecognitionOverrideEpisode {
			return errDownloadIdentityInvalid
		}
		wanted[[2]int{claim.SeasonNumber, claim.EpisodeNumber}] = struct{}{}
	}
	facts, err := transferEpisodeFactsForManifest(normalized, selected)
	if err != nil {
		return errDownloadIdentityInvalid
	}
	observed := make(map[[2]int]struct{}, len(facts))
	for _, fact := range facts {
		if fact.Season == nil || fact.Episode == nil {
			return errDownloadIdentityInvalid
		}
		if _, exists := wanted[[2]int{*fact.Season, *fact.Episode}]; !exists {
			return errDownloadIdentityInvalid
		}
		observed[[2]int{*fact.Season, *fact.Episode}] = struct{}{}
	}
	if len(observed) != len(wanted) {
		return errDownloadIdentityInvalid
	}
	return nil
}

func downloadIdentityCurrentQuery(db *gorm.DB, observed models.DownloadTask) *gorm.DB {
	return db.Model(&models.DownloadTask{}).Where(map[string]any{
		"id": observed.ID, "job_id": observed.JobID, "owner_id": observed.OwnerID,
		"identity_revision": observed.IdentityRevision, "identity_source": observed.IdentitySource, "identity_status": observed.IdentityStatus, "identity_locked": observed.IdentityLocked, "identity_snapshot_json": observed.IdentitySnapshotJSON,
		"recognition_override_tmdb_id": observed.RecognitionOverrideTMDBID, "recognition_override_media_type": observed.RecognitionOverrideMediaType, "recognition_override_season": observed.RecognitionOverrideSeason, "recognition_override_episode": observed.RecognitionOverrideEpisode,
		"follow_subscription_id": observed.FollowSubscriptionID, "follow_resource_fingerprint": observed.FollowResourceFingerprint, "completed_manifest_json": observed.CompletedManifestJSON,
		"scrape_tmdb_id": observed.ScrapeTMDBID, "scrape_media_type": observed.ScrapeMediaType, "scrape_title": observed.ScrapeTitle, "scrape_category": observed.ScrapeCategory, "scrape_season": observed.ScrapeSeason, "scrape_episode": observed.ScrapeEpisode,
	}).Where("phase <> ?", models.DownloadTaskStatusCancelled)
}

func (w *DownloadWorker) persistDownloadIdentity(observed *models.DownloadTask, identityTask models.DownloadTask, updates map[string]any) (bool, error) {
	active := false
	err := w.service.db.Transaction(func(tx *gorm.DB) error {
		if observed.IdentityLocked != identityTask.IdentityLocked {
			// Retire an old identity before inspecting its legacy intent. That
			// intent may itself have been released after a newer manual revision;
			// its absence must not decorate the new identity with an old failure.
			var current int64
			if err := downloadIdentityCurrentQuery(tx, *observed).Count(&current).Error; err != nil {
				return err
			}
			if current != 1 {
				return errDownloadIdentityChanged
			}
			manifest, exists, err := completedDownloadManifest(observed.CompletedManifestJSON)
			if err != nil || !exists {
				return invalidDownloadIdentity()
			}
			if _, err := completedDownloadIdentity(tx, *observed, manifest); err != nil {
				return err
			}
		}
		result := downloadIdentityCurrentQuery(tx, *observed).Updates(updates)
		active = result.RowsAffected == 1
		return result.Error
	})
	return active, err
}

// An invalid original intent may race with a new manual correction even before
// GetByID starts. Its failure and claim release belong only to the observed
// identity; do not pass that old error to the generic phase-only failure writer.
func (w *DownloadWorker) markDownloadIdentityFailure(observed models.DownloadTask, code, message string) (bool, error) {
	active := false
	now := time.Now().UTC()
	err := w.service.db.Transaction(func(tx *gorm.DB) error {
		result := downloadIdentityCurrentQuery(tx, observed).Updates(map[string]any{"last_error_code": safeLabel(code, 96), "last_error_message": message, "phase": models.DownloadTaskStatusFailed, "finished_at": now, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		active = result.RowsAffected == 1
		if active {
			return syncFailedFollowClaims(tx, observed.ID, now)
		}
		return nil
	})
	return active, err
}
