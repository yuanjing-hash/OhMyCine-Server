package services

import (
	"context"
	"errors"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

const catalogRecognitionFileBatch = 1000

// DirtyGeneration also advances for manual metadata/artifact edits. It is not
// the scan identity. Keep the physical baseline/source/profile fence while
// allowing newer manual deltas to be preserved by the fresh-head retry.
func catalogRecognitionScanFingerprint(library models.MediaLibrary, storage models.Storage, profile models.MediaClassificationProfile) string {
	library.DirtyGeneration = library.BaselineGeneration
	return mediaLibraryScanSourceFingerprint(library, storage, profile)
}

// pendingCatalogRecognitionBatch retains complete work evidence even when a
// large show's pending entry associations are committed in bounded chunks.
func pendingCatalogRecognitionBatch(baseline catalogScanBaseline) ([]medialibrary.RecognitionUnit, []medialibrary.File, int) {
	return (&catalogRecognitionGrouping{}).pendingBatch(baseline)
}

func (grouping *catalogRecognitionGrouping) pendingBatch(baseline catalogScanBaseline) ([]medialibrary.RecognitionUnit, []medialibrary.File, int) {
	return grouping.pendingBatchLimit(baseline, catalogRecognitionFileBatch)
}

func (grouping *catalogRecognitionGrouping) pendingBatchLimit(baseline catalogScanBaseline, limit int) ([]medialibrary.RecognitionUnit, []medialibrary.File, int) {
	pending := make(map[string]bool)
	for _, entry := range baseline.entries {
		if entry.MatchStatus == mediaRecognitionStatusPending || entry.RecognitionID == nil {
			pending[entry.RelativePath] = true
		}
	}
	all := grouping.groups(baseline)
	selected := make([]medialibrary.RecognitionUnit, 0)
	selectedFiles := make([]medialibrary.File, 0, min(limit, len(baseline.entries)))
	remaining := 0
	for _, unit := range all {
		hasPending := false
		included := false
		for _, file := range unit.Files {
			if !pending[file.RelativePath] {
				continue
			}
			hasPending = true
			if len(selectedFiles) < limit {
				selectedFiles = append(selectedFiles, file)
				included = true
			}
		}
		if hasPending {
			remaining++
		}
		if included {
			selected = append(selected, unit)
		}
	}
	return selected, selectedFiles, remaining
}

func (s *MediaLibraryService) completeCatalogRecognition(ctx context.Context, runtime JobRuntime, job ClaimedJob, payload mediaLibraryRecognitionJobPayload) error {
	if s.catalogStore == nil || s.catalogScanCommit == nil || runtime == nil || job.Job.ID == "" || job.LeaseToken == "" {
		return ErrCatalogInvalid
	}
	jobID := job.Job.ID
	jobHash := leaseHash(job.LeaseToken)
	fenceFailures := 0
	grouping := catalogRecognitionGrouping{}
	forceBase := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := runtime.Heartbeat(nil, nil, nil, nil, nil); err != nil {
			return err
		}
		if err := catalogCheckJob(s.db.WithContext(ctx), models.CatalogSnapshot{JobID: &jobID, JobLeaseHash: jobHash}, time.Now().UTC()); err != nil {
			return err
		}
		baseline, err := s.loadCatalogScanBaseline(ctx, payload.LibraryID)
		if err != nil {
			return err
		}
		var library models.MediaLibrary
		if err := s.db.WithContext(ctx).First(&library, payload.LibraryID).Error; err != nil {
			return err
		}
		var storage models.Storage
		if err := s.db.WithContext(ctx).First(&storage, library.StorageID).Error; err != nil {
			return err
		}
		var profile models.MediaClassificationProfile
		if err := s.db.WithContext(ctx).First(&profile, library.ProfileID).Error; err != nil {
			return err
		}
		var run models.MediaLibraryScanRun
		if err := s.db.WithContext(ctx).First(&run, payload.ScanRunID).Error; err != nil {
			return err
		}
		if library.BaselineGeneration != payload.Generation || run.Generation != payload.Generation || run.LibraryID != library.ID {
			return ErrCatalogFence
		}
		if run.Status == "success" {
			return nil
		}
		if run.Status != "catalog_ready" {
			return ErrCatalogFence
		}
		sourceFingerprint := catalogRecognitionScanFingerprint(library, storage, profile)
		pendingFiles := 0
		for _, entry := range baseline.entries {
			if entry.MatchStatus == mediaRecognitionStatusPending || entry.RecognitionID == nil {
				pendingFiles++
			}
		}
		kind, limit := "delta", catalogRecognitionFileBatch
		if forceBase || pendingFiles > catalogRecognitionFileBatch {
			kind, limit = "base", len(baseline.entries)
		}
		units, files, remaining := grouping.pendingBatchLimit(baseline, limit)
		run.RecognitionTotal = max(run.RecognitionTotal, run.RecognitionCompleted+remaining)
		run.RecognitionCompleted = max(0, run.RecognitionTotal-remaining)
		run.Matched, run.Unrecognized, run.RecognitionFailed = catalogRecognitionCounters(baseline)
		processed, total := int64(run.RecognitionCompleted), int64(run.RecognitionTotal)
		progress := float64(1)
		if total > 0 {
			progress = float64(processed) / float64(total)
		}
		if err := runtime.Heartbeat(&progress, &processed, &total, nil, nil); err != nil {
			return err
		}
		if remaining == 0 {
			return s.finishCatalogRecognition(ctx, baseline.head, nil, "", sourceFingerprint, jobID, jobHash, &run, profile, true)
		}
		if kind == "delta" {
			compacted, err := s.catalogStore.Compact(ctx, CatalogCompactionInput{LibraryID: payload.LibraryID, JobID: &jobID, JobLeaseHash: jobHash})
			if err != nil {
				if errors.Is(err, ErrCatalogFence) && fenceFailures < 2 {
					fenceFailures++
					continue
				}
				return err
			}
			if compacted {
				continue
			}
		}
		if err := s.catalogScanProgress(ctx, run.ID, "catalog_ready", map[string]any{"phase": "recognition_running", "recognition_total": run.RecognitionTotal}); err != nil {
			return err
		}
		// Recognizer/cache/TMDB work is outside every pinned DB transaction.
		recognized, err := s.recognizeLibraryUnitsWithExisting(ctx, library, profile, units, baseline.recognitions)
		if err != nil {
			return err
		}
		if err := runtime.Heartbeat(nil, nil, nil, nil, nil); err != nil {
			return err
		}
		candidate, token, err := s.catalogStore.BeginCandidate(ctx, CatalogCandidateInput{LibraryID: library.ID, Kind: kind, ExpectedRevision: baseline.head.Revision, SourceEpoch: baseline.head.SourceEpoch, SourceFingerprint: baseline.head.SourceFingerprint, ConfigFingerprint: baseline.head.ConfigFingerprint, JobID: &jobID, JobLeaseHash: jobHash, LeaseDuration: time.Minute})
		if err != nil {
			if errors.Is(err, ErrCatalogFence) && fenceFailures < 2 {
				fenceFailures++
				continue
			}
			return err
		}
		prepared := run
		prepared.Added, prepared.Updated, prepared.Removed = 0, 0, 0
		prepared.Matched, prepared.Unrecognized, prepared.CacheHits, prepared.RecognitionFailed = 0, 0, 0, 0
		facts, _, err := s.prepareCatalogScanFacts(ctx, candidate, token, library, storage, profile, &prepared, medialibrary.Result{Partial: true, Files: files}, baseline, units, recognized, false)
		if kind == "delta" && (err == nil || errors.Is(err, ErrCatalogBudget)) && (len(facts.Entries)+len(facts.Recognitions)+len(facts.SourceAssets) > CatalogMaxDeltaRows/2 || catalogBatchSize(facts) > CatalogMaxDeltaBytes/2) {
			// Small file counts can still carry large metadata snapshots. Do not
			// repeatedly compact/retry a delta that cannot fit the free budget.
			abandonCatalogScan(s.catalogStore, candidate.ID, token)
			forceBase = true
			continue
		}
		if err == nil {
			if kind == "base" {
				err = s.copyCatalogRecognitionBase(ctx, candidate, token, baseline.head, facts)
			} else {
				err = s.appendCatalogScanFacts(ctx, candidate, token, facts)
			}
		}
		if err == nil {
			err = s.catalogStore.Seal(ctx, candidate.ID, token)
		}
		if err == nil {
			// Preserve scan physical counters and semantic generation. Recognition
			// progress is recomputed from effective facts after each short commit.
			selectedPaths := make(map[string]bool, len(files))
			for _, file := range files {
				selectedPaths[file.RelativePath] = true
			}
			pendingPaths := make(map[string]bool)
			for _, entry := range baseline.entries {
				if entry.MatchStatus == mediaRecognitionStatusPending || entry.RecognitionID == nil {
					pendingPaths[entry.RelativePath] = true
				}
			}
			for _, item := range recognized {
				complete := true
				for _, file := range item.Unit.Files {
					if pendingPaths[file.RelativePath] && !selectedPaths[file.RelativePath] {
						complete = false
						break
					}
				}
				if complete && item.CacheHit {
					run.CacheHits++
				}
			}
			err = s.finishCatalogRecognition(ctx, baseline.head, &candidate, token, sourceFingerprint, jobID, jobHash, &run, profile, false)
		}
		if err != nil {
			abandonCatalogScan(s.catalogStore, candidate.ID, token)
			if errors.Is(err, ErrCatalogFence) && fenceFailures < 2 {
				fenceFailures++
				continue
			}
			return err
		}
		fenceFailures = 0
	}
}

func catalogRecognitionCounters(baseline catalogScanBaseline) (matched, unrecognized, failed int) {
	referenced := make(map[uint]bool)
	for _, entry := range baseline.entries {
		if entry.RecognitionID != nil {
			referenced[*entry.RecognitionID] = true
		}
	}
	for _, record := range baseline.recognitions {
		if !referenced[record.ID] || record.Status == mediaRecognitionStatusPending {
			continue
		}
		if record.Status == mediaRecognitionStatusMatched {
			matched++
		} else {
			unrecognized++
			if record.ErrorCode != "" {
				failed++
			}
		}
	}
	return
}

func (s *MediaLibraryService) finishCatalogRecognition(ctx context.Context, head models.CatalogHead, candidate *models.CatalogSnapshot, token, sourceFingerprint, jobID, jobHash string, run *models.MediaLibraryScanRun, profile models.MediaClassificationProfile, finished bool) error {
	return s.catalogStore.writeCatalogBatch(ctx, func(tx *gorm.DB) error {
		validate := func(tx *gorm.DB) error {
			var currentHead models.CatalogHead
			if err := tx.First(&currentHead, "library_id=?", head.LibraryID).Error; err != nil {
				return err
			}
			if currentHead.Mode != "versioned" || currentHead.Revision != head.Revision || currentHead.SourceEpoch != head.SourceEpoch || currentHead.SourceFingerprint != head.SourceFingerprint || currentHead.ConfigFingerprint != head.ConfigFingerprint {
				return ErrCatalogFence
			}
			var library models.MediaLibrary
			if err := tx.First(&library, head.LibraryID).Error; err != nil {
				return err
			}
			var storage models.Storage
			if err := tx.First(&storage, library.StorageID).Error; err != nil {
				return err
			}
			var currentProfile models.MediaClassificationProfile
			if err := tx.First(&currentProfile, library.ProfileID).Error; err != nil {
				return err
			}
			if library.BaselineGeneration != run.Generation || currentProfile.ID != profile.ID || currentProfile.Revision != profile.Revision || catalogRecognitionScanFingerprint(library, storage, currentProfile) != sourceFingerprint {
				return errMediaLibraryConfigurationChanged
			}
			var active models.MediaLibraryScanRun
			if err := tx.First(&active, run.ID).Error; err != nil {
				return err
			}
			if active.LibraryID != run.LibraryID || active.Generation != run.Generation || active.Status != "catalog_ready" {
				return ErrCatalogFence
			}
			return catalogCheckJob(tx, models.CatalogSnapshot{JobID: &jobID, JobLeaseHash: jobHash}, time.Now().UTC())
		}
		if candidate == nil {
			if err := validate(tx); err != nil {
				return err
			}
		} else {
			revision, err := s.catalogStore.PublishTx(tx, candidate.ID, token, head.Revision, validate)
			if err != nil {
				return err
			}
			candidate.State, candidate.PublishedRevision = "published", revision
		}
		var currentHead models.CatalogHead
		if err := tx.First(&currentHead, "library_id=?", head.LibraryID).Error; err != nil {
			return err
		}
		run.Phase, run.ErrorCode = "recognition_running", ""
		if finished {
			now := time.Now().UTC()
			run.Status, run.Phase, run.FinishedAt = "success", "completed", &now
			run.RecognitionCompleted = run.RecognitionTotal
		}
		if err := tx.Save(run).Error; err != nil {
			return err
		}
		return s.catalogScanCommit(tx, CatalogScanPublication{Candidate: candidate, Head: currentHead, Run: *run, MetadataChanged: candidate != nil, NoContentChange: candidate == nil, RecognitionOnly: true})
	})
}
