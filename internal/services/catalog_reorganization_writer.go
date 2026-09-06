package services

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	pathpkg "path"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// Only logical content and source/configuration authorize execution. Physical
// compaction may replace all layers without changing this fence.
type reorganizationCatalogFence struct {
	LibraryID         uint   `json:"library_id"`
	SourceEpoch       uint64 `json:"source_epoch"`
	SourceFingerprint string `json:"source_fingerprint"`
	ConfigFingerprint string `json:"config_fingerprint"`
	ContentRevision   uint64 `json:"content_revision"`
	Boundary          string `json:"boundary"`
}

type reorganizationCatalogState struct {
	BoundRevision            uint64                `json:"bound_revision"`
	Layers                   []CatalogBindingLayer `json:"layers"`
	Directories              map[string]string     `json:"directories,omitempty"`
	Stage                    string                `json:"stage,omitempty"`
	PublishedContentRevision uint64                `json:"published_content_revision,omitempty"`
	ManagedRevision          uint64                `json:"managed_revision,omitempty"`
	AfterManaged             int                   `json:"after_managed,omitempty"`
	ArtifactGeneration       uint64                `json:"artifact_generation,omitempty"`
	ExecutionProofVersion    int                   `json:"execution_proof_version,omitempty"`
	ExecutionStarted         bool                  `json:"execution_started,omitempty"`
}

func (s reorganizationCatalogState) binding(f reorganizationCatalogFence) CatalogSnapshotBinding {
	return CatalogSnapshotBinding{Head: models.CatalogHead{LibraryID: f.LibraryID, Mode: "versioned", Revision: s.BoundRevision, SourceEpoch: f.SourceEpoch, SourceFingerprint: f.SourceFingerprint, ConfigFingerprint: f.ConfigFingerprint}, Layers: s.Layers}
}

func reorganizationFenceTx(tx *gorm.DB, reader *CatalogReader, id uint) (*reorganizationCatalogFence, error) {
	source, err := catalogRecognitionContextTx(tx.Statement.Context, tx, reader, id)
	if err != nil {
		return nil, err
	}
	if source.Head.Mode != "versioned" {
		return nil, nil
	}
	library := source.Library
	if !library.Enabled || !source.Storage.Enabled || source.Profile.Revision != library.ProfileRevision {
		return nil, ErrCatalogFence
	}
	library.DirtyGeneration = 0
	digest := sha256.Sum256([]byte(strings.Join([]string{mediaLibraryScanSourceFingerprint(library, source.Storage, source.Profile), catalogDeletionBoundaryDigest(library, source.Storage), source.Profile.RulesJSON, libraryRuleFingerprint(library)}, "\x00")))
	return &reorganizationCatalogFence{LibraryID: id, SourceEpoch: source.Head.SourceEpoch, SourceFingerprint: source.Head.SourceFingerprint, ConfigFingerprint: source.Head.ConfigFingerprint, ContentRevision: library.ContentRevision, Boundary: fmt.Sprintf("%x", digest)}, nil
}

func (s *MediaReorganizationService) catalogRead(ctx context.Context, id uint, read func(*gorm.DB, *CatalogReader) error) error {
	if s.libraries != nil {
		return s.libraries.withCatalogRead(ctx, []uint{id}, read)
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		reader, err := PinCatalogTx(tx, []uint{id})
		if err != nil {
			return err
		}
		head, _ := reader.Head(id)
		if head.Mode == "versioned" {
			return ErrCatalogInvalid
		}
		return read(tx, reader)
	})
}

func (s *MediaReorganizationService) reorganizationCatalogMode(ctx context.Context, id uint) (bool, error) {
	versioned := false
	err := s.catalogRead(ctx, id, func(_ *gorm.DB, r *CatalogReader) error {
		h, _ := r.Head(id)
		versioned = h.Mode == "versioned"
		return nil
	})
	return versioned, err
}

func (s *MediaReorganizationService) freezeReorganizationPreview(ctx context.Context, plan *reorganizationPlan, library models.MediaLibrary, storage models.Storage, profile models.MediaClassificationProfile) error {
	return s.catalogRead(ctx, library.ID, func(tx *gorm.DB, r *CatalogReader) error {
		f, err := reorganizationFenceTx(tx, r, library.ID)
		if err != nil || f == nil {
			return err
		}
		current, err := catalogRecognitionContextTx(ctx, tx, r, library.ID)
		if err != nil {
			return err
		}
		if current.Library.ContentRevision != library.ContentRevision || mediaLibraryScanSourceFingerprint(current.Library, current.Storage, current.Profile) != mediaLibraryScanSourceFingerprint(library, storage, profile) || current.Profile.RulesJSON != profile.RulesJSON {
			return ErrCatalogFence
		}
		plan.CatalogFence = f
		var transfer models.TransferTask
		if err := tx.First(&transfer, "id=?", plan.TransferTaskID).Error; err != nil {
			return err
		}
		if transfer.ManagedRevision != plan.ManagedRevision {
			return ErrCatalogFence
		}
		return nil
	})
}

func validateReorganizationFenceTx(tx *gorm.DB, r *CatalogReader, expected *reorganizationCatalogFence) error {
	if expected == nil {
		return ErrCatalogFence
	}
	current, err := reorganizationFenceTx(tx, r, expected.LibraryID)
	if err != nil {
		return err
	}
	if current == nil || *current != *expected {
		return ErrCatalogFence
	}
	return nil
}

func (s *MediaReorganizationService) freezeReorganizationConfirmationTx(tx *gorm.DB, taskID string, id uint, plan reorganizationPlan, state *reorganizationState) error {
	r, err := PinCatalogTx(tx, []uint{id})
	if err != nil {
		return err
	}
	h, _ := r.Head(id)
	if h.Mode != "versioned" {
		if plan.CatalogFence != nil {
			return ErrCatalogFence
		}
		return nil
	}
	if s.libraries == nil || s.libraries.catalogStore == nil || plan.Version != 2 || plan.VerifiedResult == nil {
		return ErrCatalogFence
	}
	if err := validateReorganizationFenceTx(tx, r, plan.CatalogFence); err != nil {
		return err
	}
	var transfer models.TransferTask
	if err := tx.First(&transfer, "id=?", plan.TransferTaskID).Error; err != nil {
		return err
	}
	if transfer.ManagedRevision != plan.ManagedRevision {
		return ErrCatalogFence
	}
	bound, err := CaptureCatalogBindingTx(tx, id, "reorganization", taskID)
	if err != nil {
		return err
	}
	state.Catalog = &reorganizationCatalogState{BoundRevision: bound.Head.Revision, Layers: bound.Layers, Directories: map[string]string{}, ExecutionProofVersion: 1}
	return nil
}

func (w *MediaReorganizationWorker) validateCatalogExecutionTx(tx *gorm.DB, task models.MediaReorganizationTask, plan reorganizationPlan, state reorganizationState, claim ClaimedJob) error {
	return w.validateCatalogExecutionGuardTx(tx, task, plan, state, claim, nil)
}

// nil verifies the complete bounded manifest at preparation/publication; a
// selected ID verifies one imminent file operation; zero is state-only fencing.
func (w *MediaReorganizationWorker) validateCatalogExecutionGuardTx(tx *gorm.DB, task models.MediaReorganizationTask, plan reorganizationPlan, state reorganizationState, claim ClaimedJob, selected *uint) error {
	if state.Catalog == nil || plan.CatalogFence == nil || plan.Version != 2 || plan.VerifiedResult == nil || plan.LibraryID != task.LibraryID || plan.TransferTaskID != task.TransferTaskID || claim.Job.ID != task.JobID || claim.Job.OwnerID == nil || *claim.Job.OwnerID != task.OwnerID {
		return ErrCatalogFence
	}
	r, err := PinCatalogTx(tx, []uint{task.LibraryID})
	if err != nil {
		return err
	}
	fence := *plan.CatalogFence
	if state.Catalog.Stage == "reconciling" {
		var library models.MediaLibrary
		if err := tx.First(&library, task.LibraryID).Error; err != nil {
			return err
		}
		if library.ContentRevision < state.Catalog.PublishedContentRevision {
			return ErrCatalogFence
		}
		fence.ContentRevision = library.ContentRevision
	}
	if err := validateReorganizationFenceTx(tx, r, &fence); err != nil {
		return err
	}
	if w.service.queue == nil {
		return ErrCatalogInvalid
	}
	if _, err := w.service.queue.verifyLease(tx, claim.Job.ID, claim.LeaseToken); err != nil {
		return err
	}
	var current models.MediaReorganizationTask
	if err := tx.First(&current, "id=?", task.ID).Error; err != nil {
		return err
	}
	if current.OwnerID != task.OwnerID || current.JobID != task.JobID || current.PlanJSON != task.PlanJSON || current.TargetIdentityJSON != task.TargetIdentityJSON || current.ManagedManifestDigest != task.ManagedManifestDigest || current.Phase == models.MediaReorganizationPhaseCompleted {
		return ErrCatalogFence
	}
	if state.Catalog.Stage == "reconciling" {
		var persisted reorganizationState
		if decodeStrictJSON(current.StateJSON, &persisted) != nil || persisted.Catalog == nil || persisted.Catalog.Stage != "reconciling" || persisted.Catalog.AfterManaged != state.Catalog.AfterManaged || persisted.Catalog.ManagedRevision != state.Catalog.ManagedRevision {
			return ErrCatalogFence
		}
	}
	var transfer models.TransferTask
	var download models.DownloadTask
	var library models.MediaLibrary
	var storage models.Storage
	if err := tx.First(&transfer, "id=?", task.TransferTaskID).Error; err != nil {
		return err
	}
	if transfer.Phase != models.TransferTaskStatusCompleted {
		return ErrCatalogFence
	}
	expectedManaged := plan.ManagedRevision
	if state.Catalog.Stage == "reconciling" {
		expectedManaged = state.Catalog.ManagedRevision
	}
	if transfer.ManagedRevision != expectedManaged {
		return ErrCatalogFence
	}
	var transferJob models.Job
	if err := tx.First(&transferJob, "id=?", transfer.JobID).Error; err != nil {
		return err
	}
	if transferJob.Status != models.JobStatusCompleted {
		return ErrCatalogFence
	}
	if err := tx.First(&download, "id=?", transfer.DownloadTaskID).Error; err != nil {
		return err
	}
	if err := tx.First(&library, task.LibraryID).Error; err != nil {
		return err
	}
	if err := tx.First(&storage, library.StorageID).Error; err != nil {
		return err
	}
	expectedIdentity := task.SourceIdentityRevision
	if state.Catalog.Stage == "reconciling" {
		expectedIdentity = task.TargetIdentityRevision
	}
	if download.IdentityRevision != expectedIdentity || transfer.LibraryID != task.LibraryID || download.TargetStorageID == nil || *download.TargetStorageID != storage.ID || download.TargetStorageType != storage.Type || download.TargetStorageRoot != storage.RootPath || download.TargetRelativeRoot != library.RelativeRoot || download.TargetProviderRootID != library.ProviderRootID || !equalReorganizationUintPtr(download.TargetConnectionID, storage.ConnectionID) {
		return ErrCatalogFence
	}
	if selected != nil && *selected == 0 {
		_, err = PinBoundCatalogTx(tx, state.Catalog.binding(*plan.CatalogFence), "reorganization", task.ID)
		return err
	}
	var items []models.MediaManagedItem
	query := tx.Where("transfer_task_id=? AND library_id=? AND managed=? AND active=?", task.TransferTaskID, task.LibraryID, true, true)
	if selected != nil {
		query = query.Where("id=?", *selected)
	}
	if err := query.Order("id").Limit(maxReorganizationItems + 1).Find(&items).Error; err != nil {
		return err
	}
	if selected == nil && (len(items) != len(plan.Items) || managedManifestDigest(items) != task.ManagedManifestDigest) || selected != nil && len(items) != 1 {
		return ErrCatalogFence
	}
	byID := map[uint]models.MediaManagedItem{}
	for _, item := range items {
		byID[item.ID] = item
	}
	seen := map[uint]bool{}
	for _, item := range plan.Items {
		if selected != nil && item.ManagedItemID != *selected {
			continue
		}
		actual, ok := byID[item.ManagedItemID]
		if !ok || seen[item.ManagedItemID] || actual.DownloadTaskID != download.ID || actual.Kind != item.Kind || actual.Size != item.Size || actual.ProviderItemID != item.ProviderItemID || actual.ProviderParentID != item.ProviderParentID || actual.RelativePath != item.OldRelativePath || actual.IdentityRevision != task.SourceIdentityRevision {
			return ErrCatalogFence
		}
		seen[item.ManagedItemID] = true
	}
	_, err = PinBoundCatalogTx(tx, state.Catalog.binding(*plan.CatalogFence), "reorganization", task.ID)
	return err
}

type reorganizationPrepared struct {
	Head      models.CatalogHead
	Candidate models.CatalogSnapshot
	Token     string
	Facts     CatalogFactBatch
}

func (w *MediaReorganizationWorker) reorganizationMutationFacts(ctx context.Context, task models.MediaReorganizationTask, plan reorganizationPlan, state reorganizationState) (CatalogFactBatch, error) {
	var out CatalogFactBatch
	if len(plan.Items) == 0 || len(plan.Items) > maxReorganizationItems || state.Catalog == nil || plan.CatalogFence == nil {
		return out, ErrCatalogBudget
	}
	seen := map[string]bool{}
	err := w.service.libraries.catalogStore.ReadBoundCatalog(ctx, state.Catalog.binding(*plan.CatalogFence), "reorganization", task.ID, func(r *CatalogReader) error {
		for start := 0; start < len(plan.Items); start += CatalogBatchRows {
			part := plan.Items[start:min(start+CatalogBatchRows, len(plan.Items))]
			paths := make([]string, 0, len(part)*2)
			for _, item := range part {
				paths = append(paths, item.OldRelativePath, "/"+strings.TrimPrefix(item.OldRelativePath, "/"))
			}
			var entries []models.CatalogEntryFact
			var assets []models.CatalogSourceAssetFact
			if err := r.RawEntryFacts().Where("relative_path IN ?", paths).Find(&entries).Error; err != nil {
				return err
			}
			if err := r.RawSourceAssetFacts().Where("relative_path IN ? AND active=?", paths, true).Find(&assets).Error; err != nil {
				return err
			}
			for _, item := range part {
				key := strings.TrimPrefix(item.OldRelativePath, "/")
				if seen[key] {
					return ErrCatalogFence
				}
				seen[key] = true
				target, err := sanitizeTransferRelativePath(item.NewRelativePath)
				if err != nil {
					return err
				}
				if item.Kind == models.MediaManagedItemKindVideo {
					var match *models.CatalogEntryFact
					for i := range entries {
						if strings.TrimPrefix(entries[i].RelativePath, "/") == key {
							if match != nil {
								return ErrCatalogFence
							}
							match = &entries[i]
						}
					}
					if match == nil || match.Size != item.Size || match.ProviderID != item.ProviderItemID {
						return ErrCatalogFence
					}
					fact := *match
					fact.SnapshotID = ""
					fact.RelativePath = "/" + target
					fact.UpdatedAt = time.Now().UTC()
					out.Entries = append(out.Entries, fact)
				} else {
					var match *models.CatalogSourceAssetFact
					for i := range assets {
						if strings.TrimPrefix(assets[i].RelativePath, "/") == key {
							if match != nil {
								return ErrCatalogFence
							}
							match = &assets[i]
						}
					}
					if match == nil || match.Size != item.Size || match.ProviderID != item.ProviderItemID {
						return ErrCatalogFence
					}
					fact := *match
					fact.SnapshotID = ""
					fact.RelativePath = "/" + target
					fact.UpdatedAt = time.Now().UTC()
					if fact.ProviderID != "" {
						if parent := state.Catalog.Directories[reorganizationParentPath(target)]; parent != "" {
							fact.ParentProviderID = parent
						}
					}
					fact.Name = pathpkg.Base(target)
					fact.Extension = strings.ToLower(pathpkg.Ext(target))
					out.SourceAssets = append(out.SourceAssets, fact)
				}
			}
		}
		return nil
	})
	return out, err
}

// All corrected files share a new manual R, leaving every unselected physical
// version on its previous R. Raw per-file override bits are preserved verbatim.
func (w *MediaReorganizationWorker) prepareReorganization(ctx context.Context, task models.MediaReorganizationTask, plan reorganizationPlan, state reorganizationState, claim ClaimedJob) (reorganizationPrepared, error) {
	var p reorganizationPrepared
	store := w.service.libraries.catalogStore
	err := w.service.catalogRead(ctx, task.LibraryID, func(tx *gorm.DB, r *CatalogReader) error {
		if err := w.validateCatalogExecutionTx(tx, task, plan, state, claim); err != nil {
			return err
		}
		p.Head, _ = r.Head(task.LibraryID)
		return nil
	})
	if err != nil {
		return p, err
	}
	p.Facts, err = w.reorganizationMutationFacts(ctx, task, plan, state)
	if err != nil {
		return p, err
	}
	if len(p.Facts.Entries) == 0 {
		return p, ErrCatalogFence
	}
	var target MediaIdentitySnapshot
	if decodeStrictJSON(task.TargetIdentityJSON, &target) != nil || plan.VerifiedResult.TMDBID == nil || target.TMDBID == nil || *plan.VerifiedResult.TMDBID != *target.TMDBID || target.MediaType != plan.VerifiedResult.MediaType || target.Title != plan.VerifiedResult.Title || plan.VerifiedResult.Status != mediaRecognitionStatusMatched {
		return p, ErrCatalogFence
	}
	metadata, err := marshalRecognitionMetadata(*plan.VerifiedResult)
	if err != nil {
		return p, err
	}
	if len(metadata) > CatalogBatchBytes/2 {
		return p, ErrCatalogBudget
	}
	jobID := claim.Job.ID
	p.Candidate, p.Token, err = store.BeginCandidate(ctx, CatalogCandidateInput{LibraryID: task.LibraryID, Kind: "delta", ExpectedRevision: p.Head.Revision, SourceEpoch: p.Head.SourceEpoch, SourceFingerprint: p.Head.SourceFingerprint, ConfigFingerprint: p.Head.ConfigFingerprint, LeaseDuration: 2 * time.Minute, JobID: &jobID, JobLeaseHash: leaseHash(claim.LeaseToken)})
	if err != nil {
		return p, err
	}
	sourceKey := "reorganization:" + task.ID
	ids, err := store.ResolveIdentities(ctx, p.Candidate.ID, p.Token, []CatalogIdentityRequest{{Kind: "recognition", SourceKey: sourceKey}})
	if err != nil {
		return p, err
	}
	result := plan.VerifiedResult
	now := time.Now().UTC()
	var library models.MediaLibrary
	if err := w.service.db.First(&library, task.LibraryID).Error; err != nil {
		return p, err
	}
	record := models.MediaLibraryRecognition{ID: ids[0], LibraryID: task.LibraryID, SourceKey: sourceKey, InputFingerprint: task.ManagedManifestDigest, ProfileID: library.ProfileID, ProfileRevision: task.RuleRevision, Status: result.Status, Title: result.Title, MediaType: result.MediaType, TMDBID: result.TMDBID, ReleaseYear: result.ReleaseYear, Confidence: result.Confidence, CategoryName: result.CategoryName, MatchedRuleID: result.MatchedRuleID, MetadataJSON: metadata, ManualOverride: true, LastGeneration: library.BaselineGeneration, CreatedAt: now, UpdatedAt: now}
	p.Facts.Recognitions = append(p.Facts.Recognitions, CatalogRecognitionFromLegacy(record))
	oldIDs := map[uint]bool{}
	entryIDs := make([]uint, 0, len(p.Facts.Entries))
	for i := range p.Facts.Entries {
		f := &p.Facts.Entries[i]
		if f.RecognitionID != nil {
			oldIDs[*f.RecognitionID] = true
		}
		f.RecognitionID = &ids[0]
		// Explicit identity correction updates selected file overrides too, but
		// never clears their mask or changes their season/episode/version facts.
		f.MediaType, f.Title = record.MediaType, record.Title
		f.WorkKey = p.Facts.Recognitions[0].WorkKey
		f.SeriesTitle = ""
		if record.MediaType == "tv" {
			f.SeriesTitle = record.Title
		}
		f.TMDBID, f.ReleaseYear, f.MatchStatus = record.TMDBID, record.ReleaseYear, record.Status
		f.MatchConfidence, f.RecognitionErrorCode = record.Confidence, record.ErrorCode
		f.CategoryName, f.MatchedRuleID = record.CategoryName, record.MatchedRuleID
		entryIDs = append(entryIDs, f.ID)
	}
	// Do not retain orphan automatic collections through a now-unused R.
	err = store.ReadBoundCatalog(ctx, state.Catalog.binding(*plan.CatalogFence), "reorganization", task.ID, func(r *CatalogReader) error {
		ordered := make([]uint, 0, len(oldIDs))
		for id := range oldIDs {
			ordered = append(ordered, id)
		}
		if len(ordered) == 0 {
			return nil
		}
		remaining := r.Entries().Where("id NOT IN ? AND recognition_id IS NOT NULL", entryIDs).Select("recognition_id")
		var orphaned []models.MediaLibraryRecognition
		if err := r.Recognitions().Where("id IN ? AND id NOT IN (?)", ordered, remaining).Order("id").Find(&orphaned).Error; err != nil {
			return err
		}
		for _, previous := range orphaned {
			fact := CatalogRecognitionFromLegacy(previous)
			fact.Tombstone = true
			p.Facts.Recognitions = append(p.Facts.Recognitions, fact)
		}
		return nil
	})
	if err != nil {
		return p, err
	}
	if err := store.appendCatalogFactBatches(ctx, p.Candidate, p.Token, p.Facts); err != nil {
		return p, err
	}
	if err := store.Seal(ctx, p.Candidate.ID, p.Token); err != nil {
		return p, err
	}
	err = w.service.catalogRead(ctx, task.LibraryID, func(tx *gorm.DB, _ *CatalogReader) error {
		if err := w.validateCatalogExecutionTx(tx, task, plan, state, claim); err != nil {
			return err
		}
		return ValidateCatalogPreparedDeltaBudgetTx(tx, p.Candidate.ID)
	})
	return p, err
}

func (w *MediaReorganizationWorker) abandonReorganization(p *reorganizationPrepared) {
	if p.Token == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = w.service.libraries.catalogStore.Abandon(ctx, p.Candidate.ID, p.Token)
	p.Token = ""
}

func (w *MediaReorganizationWorker) persistCatalogReorganizationState(ctx context.Context, task models.MediaReorganizationTask, plan reorganizationPlan, state reorganizationState, claim ClaimedJob) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(raw) > CatalogBatchBytes {
		return ErrCatalogBudget
	}
	return w.service.libraries.catalogStore.Admission().WithForeground(ctx, func() error {
		return w.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			stateOnly := uint(0)
			if err := w.validateCatalogExecutionGuardTx(tx, task, plan, state, claim, &stateOnly); err != nil {
				return err
			}
			return tx.Model(&models.MediaReorganizationTask{}).Where("id=?", task.ID).Updates(map[string]any{"state_json": string(raw), "phase": models.MediaReorganizationPhaseExecuting, "processed_items": len(state.Completed), "last_error_code": "", "updated_at": time.Now().UTC()}).Error
		})
	})
}

func reorganizationWriteError(err error) (string, string) {
	if errors.Is(err, ErrCatalogBudget) {
		return CodeConflict, "目录合并预算不足，请稍后重试；尚未开始新的文件操作"
	}
	if errors.Is(err, ErrCatalogFence) || errors.Is(err, ErrCatalogInvalid) {
		return CodeReorganizationBoundaryChanged, "媒体、来源或任务已变化，请重新预览"
	}
	return ErrorCode(err), ErrorMessage(err)
}

func reorganizationAppError(err error) error {
	if errors.Is(err, ErrCatalogFence) || errors.Is(err, ErrCatalogBudget) || errors.Is(err, ErrCatalogInvalid) {
		code, message := reorganizationWriteError(err)
		if code == "" || code == "INTERNAL_ERROR" {
			code, message = CodeReorganizationBoundaryChanged, "媒体目录不可用，请重新预览"
		}
		return appError(code, message, err)
	}
	return err
}
