package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	catalogArtifactScopeFull        = "full"
	catalogArtifactScopeIncremental = "incremental"
)

// CatalogArtifactChangeSet is prepared from the exact before/after facts that
// authorize one Catalog publication. It is persisted with the binding in the
// publication transaction; workers never rediscover it by walking the Catalog.
type CatalogArtifactChangeSet struct {
	Entries      []uint
	Recognitions []uint
	SourceAssets []uint
}

func (c CatalogArtifactChangeSet) Empty() bool {
	return len(c.Entries) == 0 && len(c.Recognitions) == 0 && len(c.SourceAssets) == 0
}

func catalogArtifactChangesFromFacts(facts CatalogFactBatch) CatalogArtifactChangeSet {
	changes := CatalogArtifactChangeSet{}
	for _, fact := range facts.Entries {
		changes.Entries = append(changes.Entries, fact.ID)
		if fact.RecognitionID != nil {
			changes.Recognitions = append(changes.Recognitions, *fact.RecognitionID)
		}
	}
	for _, fact := range facts.Recognitions {
		changes.Recognitions = append(changes.Recognitions, fact.ID)
	}
	for _, fact := range facts.SourceAssets {
		changes.SourceAssets = append(changes.SourceAssets, fact.ID)
	}
	return normalizeCatalogArtifactChanges(changes)
}

func normalizeCatalogArtifactChanges(changes CatalogArtifactChangeSet) CatalogArtifactChangeSet {
	dedupe := func(values []uint) []uint {
		seen := make(map[uint]struct{}, len(values))
		out := make([]uint, 0, len(values))
		for _, value := range values {
			if value == 0 {
				continue
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			out = append(out, value)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	changes.Entries = dedupe(changes.Entries)
	changes.Recognitions = dedupe(changes.Recognitions)
	changes.SourceAssets = dedupe(changes.SourceAssets)
	return changes
}

// BindCatalogGenerationTx is called after head publication in the same writer.
// It performs no filesystem/network work. Pending bindings survive a crash
// before ScheduleGeneration and are the only legal input to versioned work.
func (s *MediaArtifactService) BindCatalogGenerationTx(tx *gorm.DB, libraryID uint, generation uint64) (models.CatalogArtifactBinding, error) {
	return s.bindCatalogGenerationTx(tx, libraryID, generation, catalogArtifactScopeFull, CatalogArtifactChangeSet{})
}

// BindCatalogGenerationChangesTx persists a precise incremental workset. Only
// callers holding explicit baseline/reconcile intent may request a full audit;
// absence of an older binding is not evidence that an established library
// should be expanded into full work after an upgrade.
func (s *MediaArtifactService) BindCatalogGenerationChangesTx(tx *gorm.DB, libraryID uint, generation uint64, changes CatalogArtifactChangeSet) (models.CatalogArtifactBinding, error) {
	return s.bindCatalogGenerationTx(tx, libraryID, generation, catalogArtifactScopeIncremental, normalizeCatalogArtifactChanges(changes))
}

// BindCatalogGenerationFactsTx is for mutation publishers whose immutable
// delta facts already are the exact before/after change evidence.
func (s *MediaArtifactService) BindCatalogGenerationFactsTx(tx *gorm.DB, libraryID uint, generation uint64, facts CatalogFactBatch) (models.CatalogArtifactBinding, error) {
	return s.BindCatalogGenerationChangesTx(tx, libraryID, generation, catalogArtifactChangesFromFacts(facts))
}

func (s *MediaArtifactService) bindCatalogGenerationTx(tx *gorm.DB, libraryID uint, generation uint64, requestedMode string, changes CatalogArtifactChangeSet) (models.CatalogArtifactBinding, error) {
	if s == nil || s.catalogStore == nil || generation == 0 {
		return models.CatalogArtifactBinding{}, ErrCatalogInvalid
	}
	reader, err := PinCatalogTx(tx, []uint{libraryID})
	if err != nil {
		return models.CatalogArtifactBinding{}, err
	}
	source, err := catalogRecognitionContextTx(tx.Statement.Context, tx, reader, libraryID)
	if err != nil {
		return models.CatalogArtifactBinding{}, err
	}
	if source.Head.Mode != "versioned" {
		return models.CatalogArtifactBinding{}, ErrCatalogInvalid
	}
	var existing models.CatalogArtifactBinding
	err = tx.Where("library_id = ? AND generation = ? AND head_revision = ?", libraryID, generation, source.Head.Revision).First(&existing).Error
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return existing, err
	}
	id := uuid.NewString()
	binding, err := CaptureCatalogBindingTx(tx, libraryID, "artifact", id)
	if err != nil {
		return existing, err
	}
	layers, err := json.Marshal(binding.Layers)
	if err != nil {
		return existing, err
	}
	now := time.Now().UTC()
	sourceConfig := catalogArtifactSourceFingerprint(source)
	mode := requestedMode
	var predecessor models.CatalogArtifactBinding
	if mode == catalogArtifactScopeIncremental {
		err := tx.Where("library_id = ? AND head_revision < ? AND source_epoch = ? AND source_fingerprint = ? AND config_fingerprint = ? AND source_config_fingerprint = ?", libraryID, source.Head.Revision, source.Head.SourceEpoch, source.Head.SourceFingerprint, source.Head.ConfigFingerprint, sourceConfig).Order("head_revision DESC").First(&predecessor).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return existing, err
		}
	}
	row := models.CatalogArtifactBinding{ID: id, LibraryID: libraryID, Generation: generation, HeadRevision: binding.Head.Revision, SourceEpoch: binding.Head.SourceEpoch, SourceFingerprint: binding.Head.SourceFingerprint, ConfigFingerprint: binding.Head.ConfigFingerprint, SourceConfigFingerprint: sourceConfig, LayersJSON: string(layers), ScopeMode: mode, ScopePrepared: false, State: "pending", CreatedAt: now, UpdatedAt: now}
	if err := tx.Create(&row).Error; err != nil {
		return row, err
	}
	if mode == catalogArtifactScopeIncremental {
		items := catalogArtifactBindingItems(row.ID, changes)
		if predecessor.ID != "" && predecessor.State != "completed" {
			var pending []models.CatalogArtifactBindingItem
			if err := tx.Where("binding_id = ? AND status <> ?", predecessor.ID, "completed").Find(&pending).Error; err != nil {
				return row, err
			}
			for _, item := range pending {
				item.BindingID, item.Status, item.Attempts, item.Outcome, item.ErrorCode, item.UpdatedAt = row.ID, "pending", 0, "", "", now
				items = append(items, item)
			}
		}
		if len(items) > 0 {
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(items, CatalogBatchRows).Error; err != nil {
				return row, err
			}
		}
	}
	return row, tx.Model(&models.MediaLibrary{}).Where("id = ?", libraryID).Updates(map[string]any{"artifact_generation": generation, "artifact_status": models.MediaArtifactStatusQueued, "artifact_error": "", "artifact_updated_at": now}).Error
}

func catalogArtifactBindingItems(bindingID string, changes CatalogArtifactChangeSet) []models.CatalogArtifactBindingItem {
	items := make([]models.CatalogArtifactBindingItem, 0, len(changes.Entries)+len(changes.Recognitions)+len(changes.SourceAssets))
	for _, id := range changes.Entries {
		items = append(items, models.CatalogArtifactBindingItem{BindingID: bindingID, EntityKind: "entry", EntityID: id})
	}
	for _, id := range changes.Recognitions {
		items = append(items, models.CatalogArtifactBindingItem{BindingID: bindingID, EntityKind: "recognition", EntityID: id})
	}
	for _, id := range changes.SourceAssets {
		items = append(items, models.CatalogArtifactBindingItem{BindingID: bindingID, EntityKind: "asset", EntityID: id})
	}
	return items
}

func artifactCatalogSnapshot(row models.CatalogArtifactBinding) (CatalogSnapshotBinding, error) {
	b := CatalogSnapshotBinding{Head: models.CatalogHead{LibraryID: row.LibraryID, Mode: "versioned", Revision: row.HeadRevision, SourceEpoch: row.SourceEpoch, SourceFingerprint: row.SourceFingerprint, ConfigFingerprint: row.ConfigFingerprint}}
	if len(row.LayersJSON) > 8192 {
		return b, ErrCatalogBudget
	}
	err := json.Unmarshal([]byte(row.LayersJSON), &b.Layers)
	return b, err
}

func (s *MediaArtifactService) bindScheduledArtifactPolicy(policy *mediaArtifactPolicy) error {
	if s.catalogStore == nil {
		return nil
	}
	return s.catalogStore.Read(context.Background(), []uint{policy.LibraryID}, func(reader *CatalogReader) error {
		head, _ := reader.Head(policy.LibraryID)
		if head.Mode != "versioned" {
			return nil
		}
		var row models.CatalogArtifactBinding
		if err := reader.tx.Where("library_id = ? AND generation = ?", policy.LibraryID, policy.Generation).Order("head_revision DESC").First(&row).Error; err != nil {
			return err
		}
		policy.CatalogBindingID, policy.CatalogScopeMode = row.ID, row.ScopeMode
		if row.ScopeMode == catalogArtifactScopeIncremental {
			// Exact publication-time tombstones make cleanup safe even for a
			// partial/event scan; only manifests owned by this scope are retired.
			policy.CleanupEligible = true
		}
		return s.validateArtifactBindingTx(reader.tx, *policy, nil, nil, false)
	})
}

// No canonicalization or filesystem/network work in this writer fence.
func (s *MediaArtifactService) validateArtifactBindingTx(tx *gorm.DB, policy mediaArtifactPolicy, run *models.MediaArtifactRun, claim *ClaimedJob, requireRefs bool) error {
	var row models.CatalogArtifactBinding
	if err := tx.First(&row, "id = ?", policy.CatalogBindingID).Error; err != nil {
		return err
	}
	if row.LibraryID != policy.LibraryID || row.Generation != policy.Generation || row.State == "superseded" ||
		(row.ScopeMode != catalogArtifactScopeFull && row.ScopeMode != catalogArtifactScopeIncremental) ||
		(policy.CatalogScopeMode != "" && policy.CatalogScopeMode != row.ScopeMode) {
		return ErrCatalogFence
	}
	var newest models.CatalogArtifactBinding
	if err := tx.Select("generation,head_revision").Where("library_id = ?", row.LibraryID).Order("generation DESC,head_revision DESC").First(&newest).Error; err != nil {
		return err
	}
	if newest.Generation != row.Generation || newest.HeadRevision != row.HeadRevision {
		return ErrCatalogFence
	}
	reader, err := PinCatalogTx(tx, []uint{policy.LibraryID})
	if err != nil {
		return err
	}
	source, err := catalogRecognitionContextTx(tx.Statement.Context, tx, reader, policy.LibraryID)
	if err != nil {
		return err
	}
	if !source.Library.Enabled || !source.Storage.Enabled || source.Library.ArtifactGeneration != policy.Generation || catalogArtifactSourceFingerprint(source) != row.SourceConfigFingerprint || source.Head.SourceEpoch != row.SourceEpoch || source.Head.SourceFingerprint != row.SourceFingerprint || source.Head.ConfigFingerprint != row.ConfigFingerprint {
		return ErrCatalogFence
	}
	if run != nil {
		var current models.MediaArtifactRun
		if err := tx.First(&current, "id = ?", run.ID).Error; err != nil {
			return err
		}
		if current.PolicyJSON != run.PolicyJSON || current.Generation != row.Generation || current.CatalogBindingID != row.ID || current.Status != models.MediaArtifactStatusRunning {
			return ErrCatalogFence
		}
	}
	if claim != nil {
		if s.queue == nil {
			return ErrCatalogInvalid
		}
		if _, err := s.queue.verifyLease(tx, claim.Job.ID, claim.LeaseToken); err != nil {
			return err
		}
	}
	if requireRefs {
		binding, err := artifactCatalogSnapshot(row)
		if err != nil {
			return err
		}
		_, err = PinBoundCatalogTx(tx, binding, "artifact", row.ID)
		return err
	}
	return nil
}

func catalogArtifactSourceFingerprint(source catalogRecognitionContext) string {
	// Scan admission increments dirty even when publication is a no-op. Content
	// authority comes from the exact binding/newest binding, not that counter.
	library := source.Library
	library.DirtyGeneration = 0
	stableSource := mediaLibraryScanSourceFingerprint(library, source.Storage, source.Profile)
	value := strings.Join([]string{stableSource, source.Storage.RootPath, source.Library.STRMLocalRoot, strconv.FormatBool(source.Library.MetadataArtifactsEnabled), strconv.FormatBool(source.Library.STRMEnabled), strconv.FormatBool(source.Library.SignedProxyEnabled), strconv.FormatUint(source.Library.ProfileRevision, 10), source.Profile.RulesJSON}, "\x00")
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

// Recovery is bounded and repeatable. The normal queue coalesces library work.
func (s *MediaArtifactService) RecoverCatalogArtifactBindings(ctx context.Context, limit int) error {
	if s == nil || s.catalogStore == nil {
		return nil
	}
	if limit < 1 || limit > 100 {
		return ErrCatalogBudget
	}
	var rows []models.CatalogArtifactBinding
	if err := s.catalogStore.readDB.WithContext(ctx).Where("state IN ? AND recovery_attempts < ? AND updated_at < ?", []string{"pending", "scheduled", "running", "applying", "failed"}, 5, time.Now().UTC().Add(-time.Minute)).Order("updated_at,id").Limit(limit).Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Only repair the publication-to-enqueue crash window. Once a queue job
		// exists, its retry limits/cancellation are authoritative, never revived.
		var run models.MediaArtifactRun
		findErr := s.catalogStore.readDB.WithContext(ctx).Where("library_id = ? AND catalog_binding_id = ?", row.LibraryID, row.ID).First(&run).Error
		if findErr != nil && !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		checkErr := s.catalogStore.readDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			return s.validateArtifactBindingTx(tx, mediaArtifactPolicy{LibraryID: row.LibraryID, Generation: row.Generation, CatalogBindingID: row.ID}, nil, nil, false)
		})
		if errors.Is(checkErr, ErrCatalogFence) {
			if err := s.supersedeArtifactBinding(ctx, row); err != nil {
				return err
			}
			continue
		}
		if checkErr != nil {
			return checkErr
		}
		var scheduledPolicy mediaArtifactPolicy
		if run.JobID != nil && json.Unmarshal([]byte(run.PolicyJSON), &scheduledPolicy) == nil && scheduledPolicy.CatalogBindingID == row.ID {
			if err := s.db.WithContext(ctx).Model(&row).Update("updated_at", time.Now().UTC()).Error; err != nil {
				return err
			}
			continue
		}
		if err := s.db.WithContext(ctx).Model(&row).Update("updated_at", time.Now().UTC()).Error; err != nil {
			return err
		}
		if err := s.ScheduleGeneration(row.LibraryID, row.Generation); err != nil {
			if errors.Is(err, ErrCatalogFence) {
				if e := s.supersedeArtifactBinding(ctx, row); e != nil {
					return e
				}
				continue
			}
			// A live old owner must first settle its exact file outcomes. This is
			// waiting, not a failed recovery attempt; retain the newer binding.
			var appErr *AppError
			if !errors.As(err, &appErr) || appErr.Code != CodeConflict {
				if e := s.db.WithContext(ctx).Model(&row).Update("recovery_attempts", gorm.Expr("recovery_attempts + 1")).Error; e != nil {
					return e
				}
			}
			// Rotate failing roots instead of starving all later libraries.
			s.log.Warn().Uint("library_id", row.LibraryID).Str("error_code", "artifact_binding_recovery_failed").Msg("媒体产物恢复暂未成功，保留快照等待重试")
		}
	}
	return nil
}

func (s *MediaArtifactService) supersedeArtifactBinding(ctx context.Context, row models.CatalogArtifactBinding) error {
	return s.catalogArtifactWriteTx(ctx, func(tx *gorm.DB) error {
		if err := tx.First(&row, "id = ?", row.ID).Error; err != nil {
			return err
		}
		if row.State == "completed" || row.State == "superseded" {
			return nil
		}
		binding, err := artifactCatalogSnapshot(row)
		if err != nil {
			return err
		}
		if err := ReleaseCatalogBindingTx(tx, binding, "artifact", row.ID); err != nil {
			return err
		}
		return tx.Model(&row).Updates(map[string]any{"state": "superseded", "updated_at": time.Now().UTC()}).Error
	})
}

func (s *MediaArtifactService) catalogArtifactWriteTx(ctx context.Context, write func(*gorm.DB) error) error {
	if s.catalogStore == nil {
		return s.db.WithContext(ctx).Transaction(write)
	}
	return withBackgroundTransaction(ctx, s.db, s.catalogStore.Admission(), write)
}
