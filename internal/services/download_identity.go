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
	return invalidDownloadIdentityReason("identity", "inconsistent")
}

// Only these finite classifications cross the diagnostic boundary. Neither
// Error nor the public message includes a private snapshot or upstream cause.
type downloadIdentityFailure struct {
	Stage  string
	Reason string
}

func (e *downloadIdentityFailure) Error() string { return "download identity evidence unavailable" }
func (e *downloadIdentityFailure) Unwrap() error { return errDownloadIdentityInvalid }

func invalidDownloadIdentityReason(stage, reason string) error {
	message := "媒体身份与保存的任务依据不一致，未创建自动入库任务；原任务与文件保留"
	switch reason {
	case "binding_unavailable":
		message = "原任务的追更关联依据已不可用，暂未自动恢复入库；原下载包与文件保留，无需重新下载"
	case "claim_owned_elsewhere":
		message = "所选剧集已有其他下载任务关联，暂未恢复本任务入库；请查看关联任务，原下载包保留"
	case "claim_missing", "claim_mixed_runs", "claim_set_mismatch", "run_unavailable", "run_schema_policy_unsupported", "run_target_season_mismatch":
		message = "原追更季集选择依据不完整或已变化，暂未自动恢复入库；原任务与文件保留"
	case "manifest_mismatch", "selected_package_invalid", "episode_facts_mismatch":
		message = "原下载包与保存的季集文件依据未通过核对，暂未自动恢复入库；原任务与文件保留"
	case "metadata_unavailable", "metadata_incomplete", "profile_invalid":
		message = "原分类配置或指定媒体身份尚未核实，请检查元数据与分类设置后重试；原下载包保留"
	case "database_unavailable":
		message = "原任务的恢复依据暂时无法读取，请稍后重试；原下载包与文件保留"
	}
	return appError(CodeTransferMediaUnrecognized, message, &downloadIdentityFailure{Stage: stage, Reason: reason})
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
	if task.FollowSubscriptionID == "" || task.FollowResourceFingerprint == "" {
		return task, invalidDownloadIdentityReason("legacy_identity", "binding_unavailable")
	}
	if task.IdentityRevision != 1 || task.IdentityStatus != mediaIdentityStatusVerified || task.RecognitionOverrideTMDBID == nil || task.RecognitionOverrideMediaType != "tv" || task.ScrapeMediaType != "tv" || task.RecognitionOverrideSeason == nil || !sameOptional(task.RecognitionOverrideSeason, task.ScrapeSeason) || !sameOptional(task.RecognitionOverrideEpisode, task.ScrapeEpisode) {
		return task, invalidDownloadIdentityReason("legacy_identity", "legacy_tuple_unsupported")
	}
	snapshot, err := decodeMediaIdentity(task.IdentitySnapshotJSON)
	if err != nil {
		return task, invalidDownloadIdentityReason("legacy_identity", "snapshot_invalid")
	}
	if !snapshot.Locked || validateTransferIdentityProjection(task, snapshot) != nil || !sameOptional(snapshot.Season, task.ScrapeSeason) || !sameOptional(snapshot.Episode, task.ScrapeEpisode) || !sameOptional(snapshot.Year, task.ScrapeYear) || !sameOptional(snapshot.Confidence, task.ScrapeConfidence) || !sameOptional(snapshot.TMDBID, task.RecognitionOverrideTMDBID) {
		return task, invalidDownloadIdentityReason("legacy_identity", "projection_mismatch")
	}
	stored, exists, err := completedDownloadManifest(task.CompletedManifestJSON)
	if err != nil || !exists || !manifest.Complete || stored.Name != manifest.Name || !sameAutomaticTransferFiles(stored.Files, manifest.Files) {
		return task, invalidDownloadIdentityReason("legacy_identity", "manifest_mismatch")
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
		return task, invalidDownloadIdentityReason("package_select", "selected_package_invalid")
	}
	_, expected, err := buildDownloadIdentitySnapshot(normalized, scrapeMatch{Title: task.ScrapeTitle, TMDBID: task.ScrapeTMDBID, MediaType: task.ScrapeMediaType, Category: task.ScrapeCategory, Year: task.ScrapeYear, Confidence: task.ScrapeConfidence, Season: task.ScrapeSeason, Episode: task.ScrapeEpisode}, selected, task.IdentitySource, task.IdentityStatus, false, task.IdentityRevision)
	if err != nil || expected != normalized.IdentitySnapshotJSON {
		return task, invalidDownloadIdentityReason("legacy_identity", "episode_facts_mismatch")
	}
	if err := validateLegacyFollowIntent(db, task, normalized, selected); err != nil {
		return task, err
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
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return invalidDownloadIdentityReason("claim_intent", "binding_unavailable")
		}
		return invalidDownloadIdentityReason("claim_intent", "database_unavailable")
	}
	if subscription.MediaType != "tv" || subscription.TMDBID != *original.RecognitionOverrideTMDBID || original.TargetLibraryID == nil {
		return invalidDownloadIdentityReason("claim_intent", "run_target_season_mismatch")
	}
	var claims []models.FollowEpisodeClaim
	if err := db.Where("subscription_id = ? AND resource_fingerprint = ? AND download_task_id = ?", original.FollowSubscriptionID, original.FollowResourceFingerprint, original.ID).Limit(10001).Find(&claims).Error; err != nil {
		return invalidDownloadIdentityReason("claim_intent", "database_unavailable")
	}
	if len(claims) > 0 && len(claims) <= 10000 && claims[0].RunID != nil {
		// The task-bound claim pins the accepted run. Preserve its original
		// exact released coordinates without admitting unrelated failed rows
		// from the same resource bucket. Keep all task-bound rows below so an
		// extra bound coordinate still fails the exact selected-set check.
		facts, err := transferEpisodeFactsForManifest(normalized, selected)
		if err != nil {
			return invalidDownloadIdentityReason("claim_intent", "episode_facts_mismatch")
		}
		for _, fact := range facts {
			if fact.Season == nil || fact.Episode == nil {
				continue
			}
			var released models.FollowEpisodeClaim
			err := db.Where("subscription_id = ? AND season_number = ? AND episode_number = ? AND resource_fingerprint = ? AND run_id = ? AND download_task_id IS NULL AND state = ?", original.FollowSubscriptionID, *fact.Season, *fact.Episode, original.FollowResourceFingerprint, *claims[0].RunID, "failed").First(&released).Error
			if err == nil {
				claims = append(claims, released)
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return invalidDownloadIdentityReason("claim_intent", "database_unavailable")
			}
		}
	}
	if len(claims) == 0 {
		// Retain the released-claim compatibility admitted in 1.1.97. With no
		// immutable accepted RunID on the task, do not broaden it to mixed-run
		// evidence or reconstruct a historical binding from another episode.
		if err := db.Where("subscription_id = ? AND resource_fingerprint = ? AND download_task_id IS NULL AND state = ?", original.FollowSubscriptionID, original.FollowResourceFingerprint, "failed").Limit(10001).Find(&claims).Error; err != nil {
			return invalidDownloadIdentityReason("claim_intent", "database_unavailable")
		}
	}
	if len(claims) == 0 || len(claims) > 10000 || claims[0].RunID == nil {
		reason := "claim_missing"
		facts, err := transferEpisodeFactsForManifest(normalized, selected)
		if err == nil {
			for _, fact := range facts {
				if fact.Season == nil || fact.Episode == nil {
					continue
				}
				var foreign int64
				if err := db.Model(&models.FollowEpisodeClaim{}).Where("subscription_id = ? AND season_number = ? AND episode_number = ? AND download_task_id IS NOT NULL AND download_task_id <> ?", original.FollowSubscriptionID, *fact.Season, *fact.Episode, original.ID).Count(&foreign).Error; err != nil {
					return invalidDownloadIdentityReason("claim_intent", "database_unavailable")
				}
				if foreign > 0 {
					reason = "claim_owned_elsewhere"
				}
			}
		}
		return invalidDownloadIdentityReason("claim_intent", reason)
	}
	var run models.FollowRun
	if err := db.First(&run, "id = ? AND subscription_id = ? AND owner_id = ?", *claims[0].RunID, subscription.ID, original.OwnerID).Error; err != nil {
		return invalidDownloadIdentityReason("claim_intent", "run_unavailable")
	}
	var frozen FollowExecutionSnapshot
	if run.SubscriptionRevision == 0 || decodeStrictJSON(run.ExecutionSnapshotJSON, &frozen) != nil || (frozen.Version != 1 && frozen.Version != 2) || frozen.MediaLibraryID != *original.TargetLibraryID {
		return invalidDownloadIdentityReason("claim_intent", "run_schema_policy_unsupported")
	}
	if frozen.Version == 2 && (frozen.RoutingPolicy != "source_priority" || frozen.DownloaderID != "") || frozen.Version == 1 && frozen.RoutingPolicy != "" {
		return invalidDownloadIdentityReason("claim_intent", "run_schema_policy_unsupported")
	}
	season := *original.RecognitionOverrideSeason
	seasonSelected := false
	for _, value := range frozen.Seasons {
		seasonSelected = seasonSelected || value == season
	}
	if !seasonSelected {
		return invalidDownloadIdentityReason("claim_intent", "run_target_season_mismatch")
	}
	wanted := make(map[[2]int]struct{}, len(claims))
	for _, claim := range claims {
		if claim.RunID == nil || *claim.RunID != run.ID {
			return invalidDownloadIdentityReason("claim_intent", "claim_mixed_runs")
		}
		if claim.SeasonNumber != season || claim.EpisodeNumber <= 0 || original.RecognitionOverrideEpisode != nil && claim.EpisodeNumber != *original.RecognitionOverrideEpisode {
			return invalidDownloadIdentityReason("claim_intent", "claim_set_mismatch")
		}
		wanted[[2]int{claim.SeasonNumber, claim.EpisodeNumber}] = struct{}{}
	}
	facts, err := transferEpisodeFactsForManifest(normalized, selected)
	if err != nil {
		return invalidDownloadIdentityReason("claim_intent", "episode_facts_mismatch")
	}
	observed := make(map[[2]int]struct{}, len(facts))
	for _, fact := range facts {
		if fact.Season == nil || fact.Episode == nil {
			return invalidDownloadIdentityReason("claim_intent", "episode_facts_mismatch")
		}
		if _, exists := wanted[[2]int{*fact.Season, *fact.Episode}]; !exists {
			return invalidDownloadIdentityReason("claim_intent", "claim_set_mismatch")
		}
		observed[[2]int{*fact.Season, *fact.Episode}] = struct{}{}
	}
	if len(observed) != len(wanted) {
		return invalidDownloadIdentityReason("claim_intent", "claim_set_mismatch")
	}
	return nil
}

func downloadIdentityCurrentQuery(db *gorm.DB, observed models.DownloadTask) *gorm.DB {
	return downloadIdentityObservedQuery(db, observed).Where("phase <> ?", models.DownloadTaskStatusCancelled)
}

func downloadIdentityObservedQuery(db *gorm.DB, observed models.DownloadTask) *gorm.DB {
	return db.Model(&models.DownloadTask{}).Where(map[string]any{
		"id": observed.ID, "job_id": observed.JobID, "owner_id": observed.OwnerID,
		"downloader_id": observed.DownloaderID, "provider_type": observed.ProviderType, "provider_task_id": observed.ProviderTaskID, "provider_output_id": observed.ProviderOutputID, "source_ciphertext": observed.SourceCiphertext, "source_origin": observed.SourceOrigin,
		"staging_storage_id": observed.StagingStorageID, "staging_relative_path": observed.StagingRelativePath, "staging_absolute_path": observed.StagingAbsolutePath, "staging_provider_directory_id": observed.StagingProviderDirectoryID,
		"source_data_source_json": observed.SourceDataSourceJSON, "target_data_source_json": observed.TargetDataSourceJSON,
		"target_library_id": observed.TargetLibraryID, "target_storage_id": observed.TargetStorageID, "target_storage_type": observed.TargetStorageType, "target_connection_id": observed.TargetConnectionID, "target_provider_root_id": observed.TargetProviderRootID, "target_storage_root": observed.TargetStorageRoot, "target_relative_root": observed.TargetRelativeRoot,
		"transfer_route_kind": observed.TransferRouteKind, "transfer_route_version": observed.TransferRouteVersion, "transfer_mode": observed.TransferMode, "conflict_policy": observed.ConflictPolicy,
		"movie_directory_template": observed.MovieDirectoryTemplate, "movie_filename_template": observed.MovieFilenameTemplate, "tv_directory_template": observed.TVDirectoryTemplate, "tv_filename_template": observed.TVFilenameTemplate,
		"profile_id": observed.ProfileID, "profile_revision": observed.ProfileRevision, "profile_rules_json": observed.ProfileRulesJSON, "profile_recognition_rules_json": observed.ProfileRecognitionRulesJSON, "profile_builtin_recognition_packs_json": observed.ProfileBuiltinRecognitionPacksJSON,
		"execution_location": observed.ExecutionLocation, "node_id": observed.NodeID, "route_plan_revision": observed.RoutePlanRevision, "route_plan_digest": observed.RoutePlanDigest,
		"identity_revision": observed.IdentityRevision, "identity_source": observed.IdentitySource, "identity_status": observed.IdentityStatus, "identity_locked": observed.IdentityLocked, "identity_snapshot_json": observed.IdentitySnapshotJSON,
		"recognition_override_tmdb_id": observed.RecognitionOverrideTMDBID, "recognition_override_media_type": observed.RecognitionOverrideMediaType, "recognition_override_season": observed.RecognitionOverrideSeason, "recognition_override_episode": observed.RecognitionOverrideEpisode,
		"follow_subscription_id": observed.FollowSubscriptionID, "follow_resource_fingerprint": observed.FollowResourceFingerprint, "completed_manifest_json": observed.CompletedManifestJSON,
		"scrape_tmdb_id": observed.ScrapeTMDBID, "scrape_media_type": observed.ScrapeMediaType, "scrape_title": observed.ScrapeTitle, "scrape_category": observed.ScrapeCategory, "scrape_season": observed.ScrapeSeason, "scrape_episode": observed.ScrapeEpisode,
	})
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
