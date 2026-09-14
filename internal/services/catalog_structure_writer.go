package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	pathpkg "path"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// Logical authorization deliberately excludes physical head revision. A
// compaction changes storage layout, not a confirmed user's repair decision.
type structureLogicalFence struct {
	LibraryID               uint   `json:"library_id"`
	ContentRevision         uint64 `json:"content_revision"`
	SourceEpoch             uint64 `json:"source_epoch"`
	SourceFingerprint       string `json:"source_fingerprint"`
	ConfigFingerprint       string `json:"config_fingerprint"`
	SourceConfigFingerprint string `json:"source_config_fingerprint"`
	RuleFingerprint         string `json:"rule_fingerprint"`
}

type structureCatalogRepairState struct {
	Version                  int                   `json:"version"`
	Fence                    structureLogicalFence `json:"fence"`
	BoundRevision            uint64                `json:"bound_revision"`
	Layers                   []CatalogBindingLayer `json:"layers"`
	Stage                    string                `json:"stage"`
	PublishedContentRevision uint64                `json:"published_content_revision,omitempty"`
	AfterManaged             int                   `json:"after_managed,omitempty"`
	AfterResolved            int                   `json:"after_resolved,omitempty"`
	AfterSkipped             int                   `json:"after_skipped,omitempty"`
	AfterMoves               int                   `json:"after_moves,omitempty"`
	ArtifactGeneration       uint64                `json:"artifact_generation,omitempty"`
	DiagnosisJobID           string                `json:"diagnosis_job_id,omitempty"`
	DiagnosisGeneration      uint64                `json:"diagnosis_generation,omitempty"`
	SourceRevision           uint64                `json:"source_revision,omitempty"`
	SelectionBound           bool                  `json:"selection_bound,omitempty"`
	FailedItems              int                   `json:"failed_items,omitempty"`
	BlockedItems             int                   `json:"blocked_items,omitempty"`
	OriginalTotalItems       int                   `json:"original_total_items,omitempty"`
	Cancelled                bool                  `json:"cancelled,omitempty"`
	RetryCheckpointBefore    time.Time             `json:"retry_checkpoint_before,omitempty"`
	// Only the exiting execution stack may supply cancellation authority.
	cancelPermit *CatalogPhysicalWritePermit
}

func (state structureCatalogRepairState) binding() CatalogSnapshotBinding {
	return CatalogSnapshotBinding{Head: models.CatalogHead{LibraryID: state.Fence.LibraryID, Mode: "versioned", Revision: state.BoundRevision, SourceEpoch: state.Fence.SourceEpoch, SourceFingerprint: state.Fence.SourceFingerprint, ConfigFingerprint: state.Fence.ConfigFingerprint}, Layers: state.Layers}
}

func captureStructureLogicalFenceTx(tx *gorm.DB, reader *CatalogReader, libraryID uint) (*structureLogicalFence, error) {
	source, err := catalogRecognitionContextTx(tx.Statement.Context, tx, reader, libraryID)
	if err != nil {
		return nil, err
	}
	if source.Head.Mode != "versioned" {
		return nil, nil
	}
	library := source.Library
	library.DirtyGeneration = 0
	stableSource := mediaLibraryScanSourceFingerprint(library, source.Storage, source.Profile)
	digest := sha256.Sum256([]byte(strings.Join([]string{stableSource, source.Storage.RootPath, source.Profile.RulesJSON, libraryRuleFingerprint(library)}, "\x00")))
	return &structureLogicalFence{LibraryID: libraryID, ContentRevision: library.ContentRevision, SourceEpoch: source.Head.SourceEpoch, SourceFingerprint: source.Head.SourceFingerprint, ConfigFingerprint: source.Head.ConfigFingerprint, SourceConfigFingerprint: hex.EncodeToString(digest[:]), RuleFingerprint: libraryRuleFingerprint(library)}, nil
}

func validateStructureLogicalFenceTx(tx *gorm.DB, reader *CatalogReader, expected structureLogicalFence) error {
	current, err := captureStructureLogicalFenceTx(tx, reader, expected.LibraryID)
	if err != nil {
		return err
	}
	if current == nil || *current != expected {
		return appError(CodeConflict, "媒体内容、来源或命名规则已变化，请重新预览", ErrCatalogFence)
	}
	return nil
}

func (s *MediaLibraryStructureService) freezeCatalogStructureRepairTx(tx *gorm.DB, repair *models.MediaLibraryStructureRepair, plan StructurePlan) error {
	if s.catalogStore == nil {
		return nil
	}
	reader, err := PinCatalogTx(tx, []uint{repair.LibraryID})
	if err != nil {
		return err
	}
	head, _ := reader.Head(repair.LibraryID)
	if head.Mode != "versioned" {
		return nil
	}
	if plan.catalogFence == nil {
		return appError(CodeConflict, "目录索引已变化，请重新预览", ErrCatalogFence)
	}
	var active int64
	if err := tx.Table("media_library_structure_repairs AS r").Joins("JOIN media_libraries AS l ON l.id=r.library_id").Where("r.library_id=? AND r.id<>? AND (("+readinessRepairActiveSQL+") OR (r.phase='failed' AND ("+readinessRepairFailedSQL+")))", repair.LibraryID, repair.ID).Limit(1).Count(&active).Error; err != nil {
		return err
	}
	if active > 0 {
		return appError(CodeConflict, "此媒体库已有目录修复任务，请等待完成后重试", ErrCatalogFence)
	}
	if err := validateStructureLogicalFenceTx(tx, reader, *plan.catalogFence); err != nil {
		return err
	}
	if err := validateCatalogStructureSelectionTx(tx, plan, true); err != nil {
		return err
	}
	binding, err := CaptureCatalogBindingTx(tx, repair.LibraryID, "repair", repair.ID)
	if err != nil {
		return err
	}
	state := structureCatalogRepairState{Version: 1, Fence: *plan.catalogFence, BoundRevision: binding.Head.Revision, Layers: binding.Layers, Stage: "queued", SourceRevision: plan.SourceRevision, SelectionBound: plan.SelectionBound}
	var diagnosis models.MediaLibraryStructureDiagnosis
	if err := tx.First(&diagnosis, "library_id = ?", repair.LibraryID).Error; err == nil {
		state.DiagnosisJobID, state.DiagnosisGeneration = diagnosis.JobID, diagnosis.Generation
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	repair.StateJSON = string(raw)
	return nil
}

func (s *MediaLibraryStructureService) validateCatalogStructureExecutionTx(tx *gorm.DB, repair models.MediaLibraryStructureRepair, state structureCatalogRepairState, claim *ClaimedJob, refs bool) error {
	reader, err := PinCatalogTx(tx, []uint{repair.LibraryID})
	if err != nil {
		return err
	}
	var library models.MediaLibrary
	if err := tx.First(&library, repair.LibraryID).Error; err != nil {
		return err
	}
	fence := state.Fence
	if state.Stage == "catalog_published" || state.Stage == "reconciling" {
		if library.ContentRevision < state.PublishedContentRevision {
			return ErrCatalogFence
		}
		// Physical work is already committed. A newer logical edit does not
		// invalidate exact old-generation bookkeeping; never replay the files.
		fence.ContentRevision = library.ContentRevision
	}
	if err := validateStructureLogicalFenceTx(tx, reader, fence); err != nil {
		return err
	}
	var storage models.Storage
	if err := tx.First(&storage, library.StorageID).Error; err != nil {
		return err
	}
	if !library.Enabled || !storage.Enabled {
		return ErrCatalogFence
	}
	var current models.MediaLibraryStructureRepair
	if err := tx.First(&current, "id = ?", repair.ID).Error; err != nil {
		return err
	}
	if current.PlanJSON != repair.PlanJSON || current.Phase == "completed" {
		return ErrCatalogFence
	}
	if state.Stage == "catalog_published" || state.Stage == "reconciling" {
		var published structureCatalogRepairState
		if json.Unmarshal([]byte(current.StateJSON), &published) != nil || (published.Stage != "catalog_published" && published.Stage != "reconciling") || published.PublishedContentRevision != state.PublishedContentRevision || published.ArtifactGeneration != state.ArtifactGeneration {
			return ErrCatalogFence
		}
	}
	if state.SelectionBound && state.Stage != "catalog_published" && state.Stage != "reconciling" {
		if err := validateCatalogStructureSelectionTx(tx, StructurePlan{LibraryID: repair.LibraryID, SelectionBound: true, DiagnosisJobID: state.DiagnosisJobID, DiagnosisGeneration: state.DiagnosisGeneration, SourceRevision: state.SourceRevision}, false); err != nil {
			return err
		}
	}
	if state.cancelPermit != nil {
		if !state.Cancelled {
			return ErrCatalogFence
		}
		if err := validateCancelledStructurePermitTx(tx, *state.cancelPermit, repair); err != nil {
			return err
		}
	} else if claim != nil {
		if s.queue == nil || repair.JobID == nil || *repair.JobID != claim.Job.ID {
			return ErrCatalogInvalid
		}
		if _, err := s.queue.verifyLease(tx, claim.Job.ID, claim.LeaseToken); err != nil {
			return err
		}
	} else if repair.JobID != nil {
		return ErrCatalogFence
	}
	if refs {
		_, err := PinBoundCatalogTx(tx, state.binding(), "repair", repair.ID)
		if err != nil {
			return err
		}
	}
	return nil
}

// A selection authorizes precisely the issue generation that was previewed.
// Membership is checked once in the enqueue writer; immutable diagnosis/source
// identities are then rechecked at every physical mutation boundary.
func validateCatalogStructureSelectionTx(tx *gorm.DB, plan StructurePlan, membership bool) error {
	if !plan.SelectionBound {
		return nil
	}
	generation := plan.DiagnosisGeneration
	if generation == 0 {
		generation = plan.Generation
	}
	var diagnosis models.MediaLibraryStructureDiagnosis
	var auto models.MediaLibraryStructureAutoState
	if err := tx.First(&diagnosis, "library_id = ?", plan.LibraryID).Error; err != nil {
		return err
	}
	if err := tx.First(&auto, "library_id = ?", plan.LibraryID).Error; err != nil {
		return err
	}
	if diagnosis.JobID != plan.DiagnosisJobID || diagnosis.Generation != generation || auto.SourceRevision != plan.SourceRevision || diagnosis.LastErrorCode == "source_changed" {
		return ErrCatalogFence
	}
	if !membership {
		return nil
	}
	tokens := append(append([]string(nil), plan.ResolvedIssues...), plan.SkippedIssues...)
	for start := 0; start < len(tokens); start += CatalogBatchRows {
		part := tokens[start:min(start+CatalogBatchRows, len(tokens))]
		var count int64
		if err := tx.Model(&models.MediaLibraryStructureIssue{}).Where("library_id = ? AND diagnosis_job_id = ? AND generation = ? AND token IN ?", plan.LibraryID, diagnosis.JobID, generation, part).Count(&count).Error; err != nil {
			return err
		}
		if count != int64(len(part)) {
			return ErrCatalogFence
		}
	}
	return nil
}

type catalogStructurePrepared struct {
	Head      models.CatalogHead
	Candidate models.CatalogSnapshot
	Token     string
	Facts     CatalogFactBatch
}

// Read original raw facts to preserve every SharedOverrideMask bit. Paths and
// tombstones change; effective recognition projections are never re-serialized
// into file exceptions by a structure repair.
func (s *MediaLibraryStructureService) catalogStructureMutationFacts(ctx context.Context, repair models.MediaLibraryStructureRepair, state structureCatalogRepairState, plan StructurePlan, parents map[string]string) (CatalogFactBatch, error) {
	var facts CatalogFactBatch
	if len(plan.Items)+len(plan.RecycleItems) > CatalogMaxDeltaRows {
		return facts, ErrCatalogBudget
	}
	type mutation struct {
		kind, source, target, provider string
		size, modified                 int64
		recycle, root                  bool
	}
	items := make([]mutation, 0, len(plan.Items)+len(plan.RecycleItems))
	for _, item := range plan.Items {
		items = append(items, mutation{kind: item.Kind, source: item.SourceRelative, target: item.TargetRelative, provider: item.ProviderID, size: item.Size, modified: item.ModifiedAtUnixNano, root: item.AllowProviderRootSource})
	}
	for _, item := range plan.RecycleItems {
		items = append(items, mutation{kind: item.Kind, source: item.SourceRelative, provider: item.ProviderID, size: item.Size, modified: item.ModifiedAtUnixNano, recycle: true})
	}
	seen := make(map[string]bool, len(items))
	for start := 0; start < len(items); start += CatalogBatchRows {
		part := items[start:min(start+CatalogBatchRows, len(items))]
		paths := make([]string, 0, len(part)*2)
		for _, item := range part {
			source := safeStructurePath(item.source)
			if source == "" {
				return facts, ErrCatalogInvalid
			}
			paths = append(paths, source, "/"+source)
		}
		err := s.catalogStore.ReadBoundCatalog(ctx, state.binding(), "repair", repair.ID, func(reader *CatalogReader) error {
			var entries []models.CatalogEntryFact
			var assets []models.CatalogSourceAssetFact
			if err := reader.RawEntryFacts().Where("relative_path IN ?", paths).Limit(CatalogBatchRows + 1).Find(&entries).Error; err != nil {
				return err
			}
			if err := reader.RawSourceAssetFacts().Where("active = ? AND relative_path IN ?", true, paths).Limit(CatalogBatchRows + 1).Find(&assets).Error; err != nil {
				return err
			}
			if len(entries) > CatalogBatchRows || len(assets) > CatalogBatchRows {
				return ErrCatalogFence
			}
			byEntry := map[string][]models.CatalogEntryFact{}
			byAsset := map[string][]models.CatalogSourceAssetFact{}
			for _, entry := range entries {
				key := safeStructurePath(entry.RelativePath)
				byEntry[key] = append(byEntry[key], entry)
			}
			for _, asset := range assets {
				key := safeStructurePath(asset.RelativePath)
				byAsset[key] = append(byAsset[key], asset)
			}
			for _, item := range part {
				key := safeStructurePath(item.source)
				if item.kind == "sidecar" {
					rows := byAsset[key]
					if len(rows) != 1 {
						return ErrCatalogFence
					}
					fact := rows[0]
					if !catalogStructurePhysicalFactMatches(item.provider, item.size, item.modified, fact.ProviderID, fact.Size, fact.ModifiedAt) {
						return ErrCatalogFence
					}
					identity := fmt.Sprintf("asset:%d", fact.ID)
					if seen[identity] {
						return ErrCatalogInvalid
					}
					seen[identity] = true
					fact.Tombstone = item.recycle
					if !item.recycle {
						fact.RelativePath = "/" + safeStructurePath(item.target)
						fact.Name = pathpkg.Base(item.target)
						if parent := parents[pathpkg.Dir(item.target)]; parent != "" {
							fact.ParentProviderID = parent
						}
					}
					facts.SourceAssets = append(facts.SourceAssets, fact)
				} else {
					rows := byEntry[key]
					if len(rows) == 0 && item.root {
						continue
					} // exact historical managed-root item has no catalog row
					if len(rows) != 1 {
						return ErrCatalogFence
					}
					fact := rows[0]
					if !catalogStructurePhysicalFactMatches(item.provider, item.size, item.modified, fact.ProviderID, fact.Size, fact.ModifiedAt) {
						return ErrCatalogFence
					}
					identity := fmt.Sprintf("entry:%d", fact.ID)
					if seen[identity] {
						return ErrCatalogInvalid
					}
					seen[identity] = true
					fact.Tombstone = item.recycle
					if !item.recycle {
						fact.RelativePath = "/" + safeStructurePath(item.target)
					}
					facts.Entries = append(facts.Entries, fact)
				}
			}
			return nil
		})
		if err != nil {
			return facts, err
		}
	}
	if catalogBatchSize(facts) > CatalogMaxDeltaBytes {
		return facts, ErrCatalogBudget
	}
	return facts, nil
}

func catalogStructurePhysicalFactMatches(provider string, size, modified int64, actualProvider string, actualSize int64, actualModified time.Time) bool {
	return (provider == "" || provider == actualProvider) && (size <= 0 || size == actualSize) && (modified == 0 || modified == actualModified.UTC().UnixNano())
}

func (s *MediaLibraryStructureService) prepareCatalogStructureCandidate(ctx context.Context, repair models.MediaLibraryStructureRepair, state structureCatalogRepairState, claim *ClaimedJob, facts CatalogFactBatch) (catalogStructurePrepared, error) {
	prepared := catalogStructurePrepared{Facts: facts}
	err := s.withCatalogRead(ctx, repair.LibraryID, func(tx *gorm.DB, r *CatalogReader) error {
		if err := s.validateCatalogStructureExecutionTx(tx, repair, state, claim, true); err != nil {
			return err
		}
		prepared.Head, _ = r.Head(repair.LibraryID)
		return nil
	})
	if err != nil {
		return prepared, err
	}
	if len(facts.Entries)+len(facts.SourceAssets) == 0 {
		return prepared, nil
	}
	input := CatalogCandidateInput{LibraryID: repair.LibraryID, Kind: "delta", ExpectedRevision: prepared.Head.Revision, SourceEpoch: prepared.Head.SourceEpoch, SourceFingerprint: prepared.Head.SourceFingerprint, ConfigFingerprint: prepared.Head.ConfigFingerprint, LeaseDuration: 2 * time.Minute}
	if claim != nil && state.cancelPermit == nil {
		input.JobID = &claim.Job.ID
		input.JobLeaseHash = leaseHash(claim.LeaseToken)
	}
	prepared.Candidate, prepared.Token, err = s.catalogStore.BeginCandidate(ctx, input)
	if err != nil {
		return prepared, err
	}
	if err = s.catalogStore.appendCatalogFactBatches(ctx, prepared.Candidate, prepared.Token, facts); err != nil {
		return prepared, err
	}
	if err = s.catalogStore.Seal(ctx, prepared.Candidate.ID, prepared.Token); err != nil {
		return prepared, err
	}
	err = s.withCatalogRead(ctx, repair.LibraryID, func(tx *gorm.DB, _ *CatalogReader) error {
		if err := s.validateCatalogStructureExecutionTx(tx, repair, state, claim, true); err != nil {
			return err
		}
		return ValidateCatalogPreparedDeltaBudgetTx(tx, prepared.Candidate.ID)
	})
	return prepared, err
}

func (s *MediaLibraryStructureService) abandonCatalogStructureCandidate(prepared *catalogStructurePrepared) {
	if prepared.Token == "" || s.catalogStore == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.catalogStore.Abandon(ctx, prepared.Candidate.ID, prepared.Token)
	prepared.Token = ""
}

func persistCatalogStructureStateTx(tx *gorm.DB, repairID string, state structureCatalogRepairState, updates map[string]any) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(raw) > 16*1024 {
		return ErrCatalogBudget
	}
	if updates == nil {
		updates = map[string]any{}
	}
	updates["state_json"], updates["updated_at"] = string(raw), time.Now().UTC()
	return tx.Model(&models.MediaLibraryStructureRepair{}).Where("id = ?", repairID).Updates(updates).Error
}

func (s *MediaLibraryStructureService) structureCatalogWriteTx(ctx context.Context, write func(*gorm.DB) error) error {
	var admission *CatalogWriteAdmission
	if s.catalogStore != nil {
		admission = s.catalogStore.Admission()
	}
	return withBackgroundTransaction(ctx, s.db, admission, write)
}
