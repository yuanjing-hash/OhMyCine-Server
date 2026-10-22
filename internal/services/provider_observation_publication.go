package services

import (
	"context"
	"encoding/json"
	"path"
	"strings"
	"time"

	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
)

func observationRowCurrent(row, current models.ProviderObservationItem) bool {
	if row.Fingerprint != current.Fingerprint || row.RelativePath != current.RelativePath || current.Excluded {
		return false
	}
	if row.Change == "observation_missing" {
		return current.DownlistedAt != nil && row.DownlistedAt != nil && current.DownlistedAt.Equal(*row.DownlistedAt)
	}
	return current.DownlistedAt == nil
}

// PublishPending includes positive partial-page facts, never partial absence.
// Stale batches are acknowledged without publishing outdated changes; replay
// after a crash uses this same path and cannot resurrect a removed/restored row.
func (s *ProviderObservationService) PublishPending(ctx context.Context, scopeID string, limit int) (int, error) {
	if s.publisher == nil || limit < 1 || limit > 100 {
		return 0, ErrObservationUnavailable
	}
	var scope models.ProviderObservationScope
	if err := s.db.WithContext(ctx).First(&scope, "id = ?", scopeID).Error; err != nil {
		return 0, err
	}
	var batches []models.ProviderObservationBatch
	if err := s.db.WithContext(ctx).Where("scope_id = ? AND revision > published_revision", scopeID).Order("started_at, id").Limit(limit).Find(&batches).Error; err != nil {
		return 0, err
	}
	count := 0
	for _, batch := range batches {
		if err := s.validScopeTx(s.db.WithContext(ctx), scope); err != nil {
			return count, err
		}
		var rows []models.ProviderObservationItem
		if err := s.db.WithContext(ctx).Where("scope_id = ? AND batch_id = ? AND revision > ? AND change <> ''", scopeID, batch.ID, batch.PublishedRevision).Order("revision").Limit(250).Find(&rows).Error; err != nil {
			return count, err
		}
		revision := batch.Revision
		if len(rows) == 250 {
			revision = rows[len(rows)-1].Revision
		}
		publication := ObservationPublication{Scope: scope, BatchID: batch.ID, Revision: revision}
		selected := make([]models.ProviderObservationItem, 0, len(rows))
		for _, row := range rows {
			var current models.ProviderObservationItem
			if err := observationCurrent(s.db.WithContext(ctx), scopeID).First(&current, "resource_id = ?", row.ResourceID).Error; err != nil {
				return count, err
			}
			if !observationRowCurrent(row, current) {
				continue
			}
			change := ObservationChange{Fact: observationFact(row), RelativePath: row.RelativePath, PreviousPath: row.PreviousPath, Kind: row.Change, Revision: row.Revision}
			if row.Change == "observation_missing" {
				change.MissingReason = "confirmed_observation_absence"
			}
			publication.Changes = append(publication.Changes, change)
			selected = append(selected, row)
		}
		ack := func(tx *gorm.DB) error {
			if err := s.validScopeTx(tx, scope); err != nil {
				return err
			}
			for _, row := range selected {
				// A task can acknowledge an identical fact through its own catalog
				// transaction while this publisher is preparing. A matching current
				// fingerprint alone must not resurrect that superseded delta.
				var pending models.ProviderObservationItem
				if err := tx.First(&pending, "scope_id = ? AND batch_id = ? AND resource_id = ?", scopeID, batch.ID, row.ResourceID).Error; err != nil {
					return ErrObservationDeferred
				}
				if pending.Change != row.Change || pending.Revision != row.Revision || pending.Fingerprint != row.Fingerprint || pending.RelativePath != row.RelativePath {
					return ErrObservationDeferred
				}
				var current models.ProviderObservationItem
				if err := observationCurrent(tx, scopeID).First(&current, "resource_id = ?", row.ResourceID).Error; err != nil {
					return err
				}
				if !observationRowCurrent(row, current) {
					return ErrObservationDeferred
				}
			}
			result := tx.Model(&models.ProviderObservationBatch{}).Where("id = ? AND fence = ? AND published_revision = ? AND revision >= ?", batch.ID, scope.Fence, batch.PublishedRevision, revision).Update("published_revision", revision)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrObservationDeferred
			}
			return nil
		}
		var err error
		if len(publication.Changes) == 0 || scope.Kind == "intake" {
			err = s.db.WithContext(ctx).Transaction(ack)
		} else {
			err = s.publisher.PublishObservation(ctx, publication, ack)
		}
		if err != nil {
			return count, err
		}
		if scope.Kind == "library" {
			for _, change := range publication.Changes {
				verb := map[string]string{"upsert": "巡检发现新增文件", "restored": "巡检发现文件恢复", "moved": "巡检发现文件或目录移动", "renamed": "巡检发现文件或目录改名", "updated": "巡检发现文件信息变化", "observation_missing": "两次独立复查确认文件已不在库范围，已提交下架"}[change.Kind]
				if verb == "" {
					verb = "巡检变化已提交媒体目录"
				}
				op := serverlog.OperationProviderObservation
				op.Event(s.log.Info()).Uint("library_id", scope.LibraryID).Str("batch_id", batch.ID).Str("previous_path", change.PreviousPath).Str("path", change.RelativePath).Str("change_kind", change.Kind).Int64("size", change.Fact.Size).Msg(op.Message(verb + "；后续产物按本批变化处理"))
			}
		}
		count++
	}
	return count, nil
}

type providerObservationCommitContextKey struct{}

func commitProviderObservationContextTx(ctx context.Context, tx *gorm.DB) error {
	if ctx == nil {
		return nil
	}
	ack, _ := ctx.Value(providerObservationCommitContextKey{}).(func(*gorm.DB) error)
	if ack != nil {
		return ack(tx)
	}
	return nil
}
func providerObservationScanCommit(ctx context.Context, original CatalogScanCommit) CatalogScanCommit {
	return func(tx *gorm.DB, p CatalogScanPublication) error {
		if original == nil {
			return ErrCatalogInvalid
		}
		if err := original(tx, p); err != nil {
			return err
		}
		return commitProviderObservationContextTx(ctx, tx)
	}
}

// PublishObservation reuses the existing recognition/catalog/artifact chain.
// It deliberately sets no AuthoritativeParentPaths and carries only exact,
// previously confirmed removal IDs. No provider enumeration occurs here.
func (s *MediaLibraryService) PublishObservation(ctx context.Context, p ObservationPublication, ack func(*gorm.DB) error) error {
	if ack == nil || p.Scope.Kind != "library" || p.Scope.LibraryID == 0 {
		return ErrObservationInvalid
	}
	var library models.MediaLibrary
	if err := s.db.WithContext(ctx).First(&library, p.Scope.LibraryID).Error; err != nil {
		return err
	}
	if library.ProviderRootID != p.Scope.RootID {
		return ErrObservationFence
	}
	var extras, ignores []string
	_ = json.Unmarshal([]byte(library.STRMAssetExtraExtensionsJSON), &extras)
	_ = json.Unmarshal([]byte(library.IgnorePatternsJSON), &ignores)
	videos := map[string]bool{}
	for _, ext := range defaultVideoExtensions {
		videos[strings.ToLower(ext)] = true
	}
	assets := map[string]bool{}
	for _, ext := range effectiveSourceAssetExtensions(extras) {
		assets[strings.ToLower(ext)] = true
	}
	result := medialibrary.Result{Partial: true, Scoped: true}
	for _, change := range p.Changes {
		if change.Kind == "observation_missing" {
			if change.MissingReason != "confirmed_observation_absence" {
				return ErrObservationInvalid
			}
			result.DeletedProviderIDs = append(result.DeletedProviderIDs, change.Fact.ID)
			continue
		}
		if observationIgnore(change.RelativePath, ignores) {
			continue
		}
		f := change.Fact
		if f.IsDir {
			result.Directories = append(result.Directories, cloud.TreeEntry{Item: cloud.Item{ID: f.ID, ParentID: f.ParentID, Name: f.Name, IsDir: true, ModifiedAt: f.ModifiedAt}, RelativePath: change.RelativePath})
			continue
		}
		ext := strings.ToLower(path.Ext(f.Name))
		if videos[ext] {
			result.Files = append(result.Files, medialibrary.File{RelativePath: change.RelativePath, ProviderID: f.ID, ProviderIDStable: true, Size: f.Size, ModifiedAt: f.ModifiedAt})
		} else if assets[ext] {
			result.Assets = append(result.Assets, medialibrary.SourceAsset{RelativePath: change.RelativePath, ProviderID: f.ID, ParentProviderID: f.ParentID, Name: f.Name, Extension: ext, Size: f.Size, ModifiedAt: f.ModifiedAt, HashHint: f.Digest})
		}
	}
	result.Enumerated = len(result.Files) + len(result.Assets)
	if result.Enumerated == 0 && len(result.DeletedProviderIDs) == 0 && len(result.Directories) == 0 {
		return s.db.WithContext(ctx).Transaction(ack)
	}
	catalogAck := func(tx *gorm.DB) error {
		if err := saveObservationCatalogBindingsTx(tx, p); err != nil {
			return err
		}
		return ack(tx)
	}
	ctx = context.WithValue(ctx, providerObservationCommitContextKey{}, catalogAck)
	var err error
	ctx, err = prepareObservationRecovery(ctx, s.db, p)
	if err != nil {
		return err
	}
	ctx = withProviderChangeScope(ctx, providerChangeScope{VerifiedResult: &result, ParentIDs: []string{p.Scope.RootID}})
	_, err = s.reconcile(ctx, p.Scope.LibraryID, "provider_observation")
	return err
}

func observationIgnore(relative string, patterns []string) bool {
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if match, err := path.Match(pattern, strings.TrimPrefix(relative, "/")); err == nil && match {
			return true
		}
		if match, err := path.Match(pattern, path.Base(relative)); err == nil && match {
			return true
		}
	}
	return false
}

// RecordVerifiedOperation records exact final output facts, not an API ACK.
// Transfers keep their existing ReconcileTransferBatch path. Other management
// callers can publish these facts immediately, with one supplementary check in
// five seconds. The receipt transition and facts commit together.
func (s *ProviderObservationService) RecordVerifiedOperation(ctx context.Context, receiptID, scopeID, directoryID string, facts []ObservationFact) (string, error) {
	if len(facts) == 0 || len(facts) > 250 {
		return "", ErrObservationInvalid
	}
	var batchID string
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var scope models.ProviderObservationScope
		if err := tx.First(&scope, "id = ?", scopeID).Error; err != nil {
			return err
		}
		if err := s.validScopeTx(tx, scope); err != nil {
			return err
		}
		var receipt models.QuarkOperationReceipt
		if err := tx.First(&receipt, "id = ?", receiptID).Error; err != nil {
			return err
		}
		if receipt.ConnectionID != scope.ConnectionID || (receipt.ScopeID != "" && receipt.ScopeID != scope.ID) || receipt.State != "confirmed" {
			return ErrObservationFence
		}
		if receipt.OutputBatchID != "" {
			batchID = receipt.OutputBatchID
			return nil
		}
		var dir models.ProviderObservationItem
		if err := observationCurrent(tx, scopeID).First(&dir, "resource_id = ? AND is_dir = ?", directoryID, true).Error; err != nil {
			return err
		}
		now := s.now()
		batch := models.ProviderObservationBatch{ID: receipt.ID + "-output", ScopeID: scope.ID, DirectoryID: directoryID, Fence: scope.Fence, CoverageRound: scope.CoverageRound, RuntimeID: s.runtimeID, State: "verified_operation", StartedAt: now, CompletedAt: &now, UpdatedAt: now}
		for _, fact := range facts {
			if !validObservationFact(fact, directoryID) {
				return ErrObservationInvalid
			}
			if err := s.acceptFactTx(tx, scope, dir, &batch, fact, now); err != nil {
				return err
			}
		}
		if err := tx.Create(&batch).Error; err != nil {
			return err
		}
		if err := tx.Model(&receipt).Update("output_batch_id", batch.ID).Error; err != nil {
			return err
		}
		due := now.Add(5 * time.Second)
		if err := observationCurrent(tx, scopeID).Model(&models.ProviderObservationItem{}).Where("resource_id = ? AND due_at > ?", directoryID, due).Update("due_at", due).Error; err != nil {
			return err
		}
		batchID = batch.ID
		return nil
	})
	return batchID, err
}

// The interface assertion guards the production wiring, not real-client
// acceptance of Quark playback or STRM generation.
var _ ObservationPublisher = (*MediaLibraryService)(nil)
