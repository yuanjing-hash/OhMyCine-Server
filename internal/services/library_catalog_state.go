package services

import (
	"context"
	"errors"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// Execution admission and negative catalog evidence are different facts. A
// baseline can remain complete through a committed bounded publication, while
// credentials, an unfinished physical write or unresolved recognition can still
// prevent an absence decision. This projection performs database reads only.
type libraryCatalogState struct {
	Execution       MediaLibraryReadiness
	ScanState       string
	CompleteSource  bool
	AbsenceReliable bool
	Reason          string
}

// A nil reader requests execution state only (queue/list/detail callers). A
// coverage caller supplies its already-pinned reader, keeping authorization,
// current source authority and effective membership in the same transaction.
func libraryCatalogStatesTx(tx *gorm.DB, reader *CatalogReader, ids []uint) (map[uint]libraryCatalogState, error) {
	if reader != nil && len(ids) == 0 {
		return map[uint]libraryCatalogState{}, nil
	}
	readiness, err := libraryExecutionReadinessRows(tx, ids)
	if err != nil {
		return nil, err
	}
	states := make(map[uint]libraryCatalogState, len(readiness))
	for id, execution := range readiness {
		state := libraryCatalogState{Execution: execution, ScanState: "unscanned", Reason: "baseline_unavailable"}
		if reader != nil {
			state, err = projectLibraryCatalogStateTx(tx, reader, id, state)
			if err != nil {
				return nil, err
			}
		}
		states[id] = state
	}
	return states, nil
}

func libraryReadinessRows(db *gorm.DB, ids []uint) (map[uint]MediaLibraryReadiness, error) {
	states, err := libraryCatalogStatesTx(db, nil, ids)
	if err != nil {
		return nil, err
	}
	result := make(map[uint]MediaLibraryReadiness, len(states))
	for id, state := range states {
		result[id] = state.Execution
	}
	return result, nil
}

func projectLibraryCatalogStateTx(tx *gorm.DB, reader *CatalogReader, id uint, state libraryCatalogState) (libraryCatalogState, error) {
	source, err := catalogRecognitionContextTx(context.Background(), tx, reader, id)
	if err != nil {
		return state, err
	}
	library := source.Library
	if library.BaselineGeneration == 0 || library.LastSuccessfulScanAt == nil {
		return state, nil
	}
	state.ScanState, state.Reason = "partial", "coverage_unavailable"
	var latest models.MediaLibraryScanRun
	if err := tx.Where("library_id = ?", id).Order("id DESC").First(&latest).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return state, nil
		}
		return state, err
	}
	observationSummary, observationPending, err := libraryObservationEvidenceTx(tx, source, latest)
	if err != nil {
		return state, err
	}
	if source.Head.Mode == "versioned" {
		if source.Head.SourceFingerprint != catalogSourceFingerprint(library, source.Storage) || source.Head.ConfigFingerprint != catalogConfigFingerprint(library, source.Storage, source.Profile) {
			state.Reason = "source_configuration_changed"
			return state, nil
		}
		var receipt models.CatalogScanFollowup
		if err := tx.First(&receipt, "library_id = ?", id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return state, nil
			}
			return state, err
		}
		facade := &MediaLibraryService{}
		_, run, err := facade.validateCatalogFollowupTx(tx, receipt)
		if err != nil {
			if errors.Is(err, ErrCatalogFence) || errors.Is(err, gorm.ErrRecordNotFound) {
				state.Reason = "source_configuration_changed"
				return state, nil
			}
			return state, err
		}
		if !receipt.CompleteObserved {
			return state, nil
		}
		state.CompleteSource = true
		if receipt.RecognitionPending || run.RecognitionCompleted < run.RecognitionTotal {
			state.Reason = "recognition_pending"
			return state, nil
		}
		// A later in-flight/failed enumeration is not a committed bounded delta.
		// A summary with no publication cannot override the durable receipt.
		if latest.ID != run.ID && !observationSummary && (latest.CatalogPublishedAt == nil || latest.Generation != library.BaselineGeneration || !scanMatchesCatalogSource(source, latest)) {
			state.Reason = "scan_pending"
			return state, nil
		}
	} else {
		// Legacy catalogs have no coalesced receipt. Require an actual complete
		// successful enumeration whose frozen source still matches today; a
		// baseline counter or a display timestamp alone is insufficient.
		var complete models.MediaLibraryScanRun
		err := tx.Where("library_id = ? AND generation > 0 AND generation <= ? AND status = ? AND partial = ?", id, library.BaselineGeneration, "success", false).Order("id DESC").First(&complete).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return state, nil
			}
			return state, err
		}
		if !scanMatchesCatalogSource(source, complete) || library.ProfileRevision != source.Profile.Revision {
			state.Reason = "source_configuration_changed"
			return state, nil
		}
		state.CompleteSource = true
	}
	if (!observationSummary && latest.Status != "success") || latest.RecognitionCompleted < latest.RecognitionTotal {
		state.Reason = "scan_pending"
		return state, nil
	}
	// Read-only diagnosis does not change membership. Physical execution and
	// unavailable sources do; use the shared execution evidence for those gates.
	if !state.Execution.Ready && state.Execution.ReadinessStatus != "checking" {
		state.Reason = state.Execution.ReadinessStatus
		return state, nil
	}
	if observationPending {
		state.Reason = "publication_pending"
		return state, nil
	}
	state.ScanState, state.AbsenceReliable, state.Reason = "complete", true, ""
	return state, nil
}

// An established full source remains useful while its same-scope observer is
// enumerating again. Only known unresolved facts revoke absence authority;
// enumeration, account admission and hot/cold scheduling do not.
func libraryObservationEvidenceTx(tx *gorm.DB, source catalogRecognitionContext, latest models.MediaLibraryScanRun) (bool, bool, error) {
	var scopes []models.ProviderObservationScope
	if err := tx.Where("library_id = ? AND kind = ? AND enabled = ?", source.Library.ID, "library", true).Limit(2).Find(&scopes).Error; err != nil {
		return false, false, err
	}
	if len(scopes) == 0 {
		return false, false, nil
	}
	if len(scopes) != 1 {
		return false, true, nil
	}
	scope := scopes[0]
	root := source.Library.ProviderRootID
	if root == "" {
		root = source.Storage.RootPath
	}
	if source.Storage.ConnectionID == nil || scope.ConnectionID != *source.Storage.ConnectionID || scope.Provider != source.Storage.Type || scope.RootID != root || scope.Fence == "" || scope.AccountKey == "" {
		return false, true, nil
	}
	if err := observationLibrarySourceCurrentTx(tx, scope); err != nil {
		if errors.Is(err, ErrObservationFence) || errors.Is(err, gorm.ErrRecordNotFound) {
			return false, true, nil
		}
		return false, false, err
	}
	if scope.LastErrorCode != "" {
		return false, true, nil
	}
	if err := observationMutationHoldTx(tx, scope); err != nil {
		if errors.Is(err, ErrObservationDeferred) {
			return false, true, nil
		}
		return false, false, err
	}
	observer := &ProviderObservationService{}
	writes, err := observer.scopeHasPendingWritesTx(tx, scope.ID)
	if err != nil || writes {
		return false, writes, err
	}
	var pending int64
	if err := tx.Model(&models.ProviderObservationBatch{}).Where("scope_id = ? AND revision > published_revision", scope.ID).Count(&pending).Error; err != nil || pending > 0 {
		return false, pending > 0, err
	}
	if err := observationCurrent(tx, scope.ID).Model(&models.ProviderObservationItem{}).Where("excluded = ? AND (path_repair_pending = ? OR (missing_since IS NOT NULL AND downlisted_at IS NULL))", false, true).Count(&pending).Error; err != nil || pending > 0 {
		return false, pending > 0, err
	}
	// Confirmation can be paged even when all facts are unchanged. Check the
	// actual unobserved current children rather than treating its phase as a
	// discovered change; include scope-wide omissions once traversal is done.
	if err := tx.Table("provider_observation_batches b").Where("b.scope_id = ? AND b.fence = ? AND b.state = ?", scope.ID, scope.Fence, "confirming").Where(`EXISTS (
		SELECT 1 FROM provider_observation_items i WHERE i.scope_id=b.scope_id AND i.batch_id='' AND i.excluded=0 AND i.downlisted_at IS NULL
		AND ((i.parent_id=b.directory_id AND i.seen_at<=b.started_at AND NOT EXISTS (SELECT 1 FROM provider_observation_items seen WHERE seen.scope_id=b.scope_id AND seen.batch_id=b.id AND seen.resource_id=i.resource_id))
		OR (b.confirmation_phase='scope_missing' AND i.resource_id<>? AND i.last_seen_round<b.coverage_round AND i.seen_at<?)))`, scope.RootID, scope.CoverageStartedAt).Count(&pending).Error; err != nil || pending > 0 {
		return false, pending > 0, err
	}
	summary := scope.RequestedScanRunID == latest.ID && latest.SourceFingerprint == scope.Fence && latest.ErrorCode == "" && latest.Status != "failed" && latest.Status != "cancelled" && scope.LastCompleteAt != nil
	return summary, false, nil
}

func scanMatchesCatalogSource(source catalogRecognitionContext, run models.MediaLibraryScanRun) bool {
	if run.SourceFingerprint == "" || run.Generation == 0 {
		return false
	}
	// Ordinary scan allocation freezes dirty+1; a no-op retains the current
	// generation. Both are real producer shapes, with every source field intact.
	library := source.Library
	if mediaLibraryScanSourceFingerprint(library, source.Storage, source.Profile) == run.SourceFingerprint {
		return true
	}
	library.DirtyGeneration = run.Generation - 1
	return mediaLibraryScanSourceFingerprint(library, source.Storage, source.Profile) == run.SourceFingerprint
}
