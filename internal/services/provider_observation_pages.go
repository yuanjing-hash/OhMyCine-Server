package services

import (
	"context"
	"errors"
	"path"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func (s *ProviderObservationService) acceptPage(ctx context.Context, scope models.ProviderObservationScope, dir models.ProviderObservationItem, batch *models.ProviderObservationBatch, page ObservationDirectoryPage) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.validScopeTx(tx, scope); err != nil {
			return err
		}
		var live models.ProviderObservationBatch
		if err := tx.First(&live, "id = ?", batch.ID).Error; err != nil {
			return err
		}
		if live.State != "reading" || live.Offset != batch.Offset || live.RuntimeID != s.runtimeID {
			return ErrObservationFence
		}
		if err := observationCurrent(tx, scope.ID).First(&dir, "resource_id = ?", dir.ResourceID).Error; err != nil {
			return err
		}
		if dir.ActiveBatchID != batch.ID {
			return ErrObservationFence
		}
		now := s.now()
		invalid := ""
		if page.Offset != live.Offset || len(page.Items) > observationPageLimit || (page.HasMore && len(page.Items) == 0) {
			invalid = "paging_discontinuity"
		}
		if page.Total != nil {
			if *page.Total < 0 || (live.ExpectedTotal != nil && *live.ExpectedTotal != *page.Total) {
				invalid = "paging_total_changed"
			}
			if live.ExpectedTotal == nil {
				v := *page.Total
				live.ExpectedTotal = &v
			}
		}
		if page.Partial {
			invalid = "provider_partial"
		}
		for _, f := range page.Items {
			if !validObservationFact(f, dir.ResourceID) {
				invalid = "invalid_directory_entry"
			}
		}
		if invalid != "paging_discontinuity" && invalid != "invalid_directory_entry" {
			// Reject the whole stale page before accepting any facts. Completeness
			// must restart at page zero, including a rename within the same parent.
			for _, f := range page.Items {
				var current models.ProviderObservationItem
				if err := observationCurrent(tx, scope.ID).First(&current, "resource_id = ?", f.ID).Error; err != nil {
					if !errors.Is(err, gorm.ErrRecordNotFound) {
						return err
					}
				} else if observationFactSuperseded(current, dir, live, f) {
					invalid = "observation_fact_superseded"
					break
				}
			}
		}
		if invalid == "paging_discontinuity" || invalid == "invalid_directory_entry" || invalid == "observation_fact_superseded" {
			live.State = "incomplete"
			live.IncompleteReason = invalid
		} else {
			for _, f := range page.Items {
				var seen int64
				if err := tx.Model(&models.ProviderObservationItem{}).Where("scope_id = ? AND batch_id = ? AND resource_id = ?", scope.ID, live.ID, f.ID).Count(&seen).Error; err != nil {
					return err
				}
				if seen > 0 {
					invalid = "paging_duplicate_identity"
				}
				if err := s.acceptFactTx(tx, scope, dir, &live, f, now); err != nil {
					return err
				}
			}
			live.Offset += int64(len(page.Items))
			live.SeenCount += int64(len(page.Items))
			live.Pages++
			if live.ExpectedTotal != nil && (live.Offset > *live.ExpectedTotal || (!page.HasMore && live.Offset != *live.ExpectedTotal)) {
				invalid = "paging_total_mismatch"
			}
			if invalid != "" {
				live.State = "incomplete"
				live.IncompleteReason = invalid
			} else if !page.HasMore {
				live.State = "confirming"
				live.CompletedAt = &now
				if err := s.completeDirectoryTx(tx, scope, &dir, &live, now); err != nil {
					return err
				}
			}
		}
		live.UpdatedAt = now
		if live.State != "incomplete" {
			if err := tx.Model(&models.ProviderObservationScope{}).Where("id = ? AND last_error_code IN ?", scope.ID, []string{"read_failed", "observation_read_failed", "paging_discontinuity", "paging_total_changed", "provider_partial", "invalid_directory_entry", "paging_duplicate_identity", "paging_total_mismatch", "observation_fact_superseded"}).Updates(map[string]any{"last_error_code": "", "next_retry_at": nil}).Error; err != nil {
				return err
			}
		}
		if err := tx.Save(&live).Error; err != nil {
			return err
		}
		if live.State == "incomplete" {
			if err := tx.Model(&models.ProviderObservationScope{}).Where("id = ?", scope.ID).Updates(map[string]any{"last_error_code": live.IncompleteReason, "next_retry_at": now.Add(time.Minute)}).Error; err != nil {
				return err
			}
			if err := observationCurrent(tx, scope.ID).Model(&models.ProviderObservationItem{}).Where("resource_id = ?", dir.ResourceID).Updates(map[string]any{"active_batch_id": "", "due_at": now.Add(time.Minute)}).Error; err != nil {
				return err
			}
		}
		*batch = live
		return nil
	})
}

func observationFactSuperseded(current, dir models.ProviderObservationItem, batch models.ProviderObservationBatch, fact ObservationFact) bool {
	return current.SeenAt.After(batch.StartedAt) && (current.Fingerprint != observationFingerprint(fact) || current.RelativePath != path.Join(dir.RelativePath, fact.Name))
}

func (s *ProviderObservationService) acceptFactTx(tx *gorm.DB, scope models.ProviderObservationScope, dir models.ProviderObservationItem, batch *models.ProviderObservationBatch, f ObservationFact, now time.Time) error {
	var old models.ProviderObservationItem
	err := observationCurrent(tx, scope.ID).First(&old, "resource_id = ?", f.ID).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	// Newer positive facts protect both cross-parent moves and same-parent
	// renames; an old page is not evidence to undo a confirmed operation.
	if err == nil && observationFactSuperseded(old, dir, *batch, f) {
		return ErrObservationDeferred
	}
	fingerprint := observationFingerprint(f)
	relative := path.Join(dir.RelativePath, f.Name)
	changed := errors.Is(err, gorm.ErrRecordNotFound) || old.Fingerprint != fingerprint || old.RelativePath != relative || old.DownlistedAt != nil
	row := old
	row.ScopeID = scope.ID
	row.BatchID = ""
	row.ResourceID = f.ID
	row.ParentID = f.ParentID
	row.Name = f.Name
	row.IsDir = f.IsDir
	row.Size = f.Size
	row.ModifiedAt = f.ModifiedAt
	row.ContentVersion = f.ContentVersion
	row.Digest = f.Digest
	row.RelativePath = relative
	row.Fingerprint = fingerprint
	row.SeenAt = now
	row.LastSeenRound = scope.CoverageRound
	row.UpdatedAt = now
	row.MissingSince = nil
	row.MissingLastAt = nil
	row.MissingObservations = 0
	row.MissingRound = 0
	row.MissingBatchID = ""
	row.DownlistedAt = nil
	row.RecoveryUntil = nil
	row.AbsentParentRound = 0
	row.Excluded = old.Excluded || dir.Excluded || observationExcluded(f.Name, scope.Kind == "intake")
	if changed {
		row.StableSince = now
		row.StableObservations = 0
		row.LastStableBatchID = ""
		hot := now.Add(30 * time.Minute)
		row.HotUntil = &hot
	}
	if f.IsDir {
		if errors.Is(err, gorm.ErrRecordNotFound) || old.DownlistedAt != nil || old.AbsentParentRound > 0 || old.ParentID != f.ParentID || (scope.CoverageRound > old.QueueRound && scope.LastCompleteAt != nil && scope.LastCompleteAt.Before(scope.CoverageStartedAt)) || scope.LastCompleteAt == nil {
			row.QueueRound = scope.CoverageRound
			row.DueAt = now
			if old.AbsentParentRound > 0 || old.ParentID != f.ParentID {
				row.CompletedRound = min(row.CompletedRound, scope.CoverageRound-1)
			}
		}
		if row.DueAt.IsZero() {
			row.DueAt = now
		}
		if old.RelativePath != "" && old.RelativePath != relative {
			row.PathRepairPending = true
			row.PathRepairCursor = ""
		}
	}
	if err = observationUpsert(tx, &row); err != nil {
		return err
	}
	observed := row
	observed.BatchID = batch.ID
	observed.PreviousPath = old.RelativePath
	observed.Change = ""
	observed.Revision = 0
	// A duplicate provider page invalidates completeness, but must not erase an
	// earlier positive delta that has not yet reached the catalog.
	if !changed {
		var prior models.ProviderObservationItem
		if err := tx.First(&prior, "scope_id = ? AND batch_id = ? AND resource_id = ?", scope.ID, batch.ID, f.ID).Error; err == nil {
			observed.Change, observed.Revision, observed.PreviousPath = prior.Change, prior.Revision, prior.PreviousPath
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
	}
	if changed && !row.Excluded {
		// Directory rows, rather than their child files, are what the scheduler
		// reads. A move also makes the old parent worth checking again.
		if err := observationHeatParentsTx(tx, scope.ID, row.ParentID, old.ParentID, now); err != nil {
			return err
		}
		batch.Revision++
		observed.Revision = batch.Revision
		observed.Change = "upsert"
		if old.DownlistedAt != nil {
			observed.Change = "restored"
		} else if old.ParentID != "" && old.ParentID != row.ParentID {
			observed.Change = "moved"
		} else if old.Name != "" && old.Name != row.Name {
			observed.Change = "renamed"
		} else if old.ResourceID != "" {
			observed.Change = "updated"
		}
	}
	return observationUpsert(tx, &observed)
}

func observationHeatParentsTx(tx *gorm.DB, scopeID, parentID, previousParentID string, now time.Time) error {
	hotUntil, due := now.Add(30*time.Minute), now.Add(2*time.Minute)
	// Only advance the next check. In particular, preserve an immediate page
	// continuation or an earlier five-second operation reconciliation, and do
	// not reset the active batch, coverage queue, or an absent branch.
	return observationCurrent(tx, scopeID).Model(&models.ProviderObservationItem{}).
		Where("resource_id IN ? AND is_dir = ? AND excluded = ? AND absent_parent_round = 0 AND downlisted_at IS NULL", []string{parentID, previousParentID}, true, false).
		Updates(map[string]any{
			"hot_until":  gorm.Expr("CASE WHEN hot_until IS NULL OR hot_until < ? THEN ? ELSE hot_until END", hotUntil, hotUntil),
			"due_at":     gorm.Expr("CASE WHEN due_at > ? THEN ? ELSE due_at END", due, due),
			"updated_at": now,
		}).Error
}

func (s *ProviderObservationService) scopeHasPendingWritesTx(tx *gorm.DB, scopeID string) (bool, error) {
	var count int64
	err := tx.Model(&models.QuarkOperationReceipt{}).Where("(scope_id = ? OR (scope_id = '' AND connection_id = (SELECT connection_id FROM provider_observation_scopes WHERE id = ?))) AND state NOT IN ?", scopeID, scopeID, []string{"confirmed", "catalog_published", "artifacts_ready", "rejected", "failed"}).Count(&count).Error
	return count > 0, err
}

// completeDirectoryTx advances at most 250 local identities per transaction.
// Once pagination is complete its immutable evidence may resume after restart;
// an unfinished provider traversal still restarts at page zero.
func (s *ProviderObservationService) completeDirectoryTx(tx *gorm.DB, scope models.ProviderObservationScope, dir *models.ProviderObservationItem, batch *models.ProviderObservationBatch, now time.Time) error {
	remaining := 250
	if batch.ConfirmationPhase == "" {
		batch.ConfirmationPhase = "stable"
	}
	for remaining > 0 {
		pending, err := s.scopeHasPendingWritesTx(tx, scope.ID)
		if err != nil {
			return err
		}
		switch batch.ConfirmationPhase {
		case "stable":
			var facts []models.ProviderObservationItem
			if err = tx.Where("scope_id = ? AND batch_id = ? AND resource_id > ?", scope.ID, batch.ID, batch.ConfirmationCursor).Order("resource_id").Limit(remaining).Find(&facts).Error; err != nil {
				return err
			}
			for _, fact := range facts {
				if err = observationCurrent(tx, scope.ID).Model(&models.ProviderObservationItem{}).Where("resource_id = ? AND fingerprint = ? AND last_stable_batch_id <> ?", fact.ResourceID, fact.Fingerprint, batch.ID).Updates(map[string]any{"stable_observations": gorm.Expr("stable_observations + 1"), "last_stable_batch_id": batch.ID}).Error; err != nil {
					return err
				}
				batch.ConfirmationCursor = fact.ResourceID
			}
			exhausted := len(facts) < remaining
			remaining -= len(facts)
			if exhausted {
				batch.ConfirmationPhase, batch.ConfirmationCursor = "directory_missing", ""
			}
		case "directory_missing":
			var absent []models.ProviderObservationItem
			if !pending {
				if err = observationCurrent(tx, scope.ID).Where("parent_id = ? AND excluded = ? AND seen_at <= ? AND resource_id > ? AND resource_id NOT IN (SELECT resource_id FROM provider_observation_items WHERE scope_id = ? AND batch_id = ?)", dir.ResourceID, false, batch.StartedAt, batch.ConfirmationCursor, scope.ID, batch.ID).Order("resource_id").Limit(remaining).Find(&absent).Error; err != nil {
					return err
				}
			}
			for i := range absent {
				if err = s.markMissingTx(tx, &absent[i], batch, now); err != nil {
					return err
				}
				if absent[i].IsDir {
					// The provider did not list this directory in its complete
					// parent. Do not keep polling its inaccessible ID forever,
					// and do not claim its children were actually enumerated.
					if err = observationCurrent(tx, scope.ID).Model(&models.ProviderObservationItem{}).Where("resource_id = ?", absent[i].ResourceID).Update("absent_parent_round", scope.CoverageRound).Error; err != nil {
						return err
					}
				}
				batch.ConfirmationCursor = absent[i].ResourceID
			}
			exhausted := len(absent) < remaining
			remaining -= len(absent)
			if !exhausted {
				continue
			}
			// Child facts can heat this directory in the same transaction, and
			// a resumed confirmation may have an earlier supplementary check.
			// Reload before the full upsert so neither state is overwritten.
			if err = observationCurrent(tx, scope.ID).First(dir, "resource_id = ?", dir.ResourceID).Error; err != nil {
				return err
			}
			dir.ActiveBatchID = ""
			dir.LastCompleteAt = batch.CompletedAt
			dir.CompletedRound = max(dir.CompletedRound, batch.CoverageRound)
			dir.UpdatedAt = now
			interval := 24 * time.Hour
			if scope.Kind == "intake" {
				interval = time.Minute
			} else if dir.HotUntil != nil && dir.HotUntil.After(now) {
				interval = 2 * time.Minute
			} else if dir.ResourceID == scope.RootID {
				interval = 15 * time.Minute
			}
			due := now.Add(interval)
			if dir.DueAt.After(now) && dir.DueAt.Before(due) {
				due = dir.DueAt
			}
			dir.DueAt = due
			if err = observationUpsert(tx, dir); err != nil {
				return err
			}
			batch.ConfirmationPhase, batch.ConfirmationCursor = "unreachable_branches", ""
		case "unreachable_branches":
			// Propagate only existing parent-graph evidence, at most 250 rows
			// per transaction. A move discovered elsewhere clears this marker
			// and queues the branch again before coverage can finish.
			var children []models.ProviderObservationItem
			if err = observationCurrent(tx, scope.ID).Where("is_dir = ? AND excluded = ? AND absent_parent_round = 0 AND parent_id IN (SELECT resource_id FROM provider_observation_items WHERE scope_id = ? AND batch_id = '' AND absent_parent_round > 0)", true, false, scope.ID).Order("resource_id").Limit(remaining).Find(&children).Error; err != nil {
				return err
			}
			for _, child := range children {
				if err = observationCurrent(tx, scope.ID).Model(&models.ProviderObservationItem{}).Where("resource_id = ?", child.ResourceID).Update("absent_parent_round", scope.CoverageRound).Error; err != nil {
					return err
				}
			}
			remaining -= len(children)
			if len(children) > 0 {
				continue
			}
			var left int64
			if err = observationCurrent(tx, scope.ID).Model(&models.ProviderObservationItem{}).Where("is_dir = ? AND excluded = ? AND absent_parent_round = 0 AND queue_round = ? AND completed_round < ?", true, false, scope.CoverageRound, scope.CoverageRound).Count(&left).Error; err != nil {
				return err
			}
			var current models.ProviderObservationScope
			if err = tx.First(&current, "id = ?", scope.ID).Error; err != nil {
				return err
			}
			if left > 0 || (current.LastCompleteAt != nil && !current.LastCompleteAt.Before(current.CoverageStartedAt)) {
				batch.ConfirmationPhase, batch.State = "done", "complete"
				return nil
			}
			batch.ConfirmationPhase, batch.ConfirmationCursor = "scope_missing", ""
		case "scope_missing":
			var missing []models.ProviderObservationItem
			if !pending {
				if err = observationCurrent(tx, scope.ID).Where("resource_id <> ? AND last_seen_round < ? AND excluded = ? AND seen_at < ? AND resource_id > ?", scope.RootID, scope.CoverageRound, false, scope.CoverageStartedAt, batch.ConfirmationCursor).Order("resource_id").Limit(remaining).Find(&missing).Error; err != nil {
					return err
				}
			}
			for i := range missing {
				item := &missing[i]
				batch.ConfirmationCursor = item.ResourceID
				if err = s.markMissingTx(tx, item, batch, now); err != nil {
					return err
				}
				if item.DownlistedAt != nil || item.MissingObservations < 2 || item.MissingSince == nil || now.Sub(*item.MissingSince) < observationMissingInterval {
					continue
				}
				item.DownlistedAt = &now
				until := now.Add(observationRecoveryRetention)
				item.RecoveryUntil, item.UpdatedAt = &until, now
				if err = observationUpsert(tx, item); err != nil {
					return err
				}
				gone := *item
				gone.BatchID, gone.Change, gone.PreviousPath = batch.ID, "observation_missing", item.RelativePath
				batch.Revision++
				gone.Revision = batch.Revision
				if err = observationUpsert(tx, &gone); err != nil {
					return err
				}
			}
			exhausted := len(missing) < remaining
			remaining -= len(missing)
			if exhausted {
				batch.ConfirmationPhase, batch.State = "done", "complete"
				return tx.Model(&models.ProviderObservationScope{}).Where("id = ? AND coverage_round = ?", scope.ID, batch.CoverageRound).Updates(map[string]any{"last_complete_at": now, "last_error_code": "", "next_retry_at": nil, "updated_at": now}).Error
			}
		default:
			return ErrObservationInvalid
		}
	}
	batch.State = "confirming"
	return nil
}

func (s *ProviderObservationService) resumeConfirmation(ctx context.Context, scope models.ProviderObservationScope) (bool, error) {
	found := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.validScopeTx(tx, scope); err != nil {
			return err
		}
		var batch models.ProviderObservationBatch
		err := tx.Where("scope_id = ? AND fence = ? AND state = 'confirming'", scope.ID, scope.Fence).Order("started_at, id").First(&batch).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		var dir models.ProviderObservationItem
		if err = observationCurrent(tx, scope.ID).First(&dir, "resource_id = ?", batch.DirectoryID).Error; err != nil {
			return err
		}
		if err = s.completeDirectoryTx(tx, scope, &dir, &batch, s.now()); err != nil {
			return err
		}
		batch.UpdatedAt = s.now()
		return tx.Save(&batch).Error
	})
	return found, err
}

func (s *ProviderObservationService) markMissingTx(tx *gorm.DB, item *models.ProviderObservationItem, batch *models.ProviderObservationBatch, now time.Time) error {
	if item.DownlistedAt != nil {
		return nil
	}
	if item.MissingSince == nil {
		item.MissingSince = &now
		item.MissingLastAt = &now
		item.MissingObservations = 1
		item.MissingRound = batch.CoverageRound
		item.MissingBatchID = batch.ID
	} else if item.MissingBatchID != batch.ID && item.MissingLastAt != nil && now.Sub(*item.MissingLastAt) >= observationMissingInterval {
		item.MissingObservations++
		item.MissingLastAt = &now
		item.MissingRound = batch.CoverageRound
		item.MissingBatchID = batch.ID
	}
	item.UpdatedAt = now
	if err := observationUpsert(tx, item); err != nil {
		return err
	}
	// A candidate requests a new scope-wide pass after the minimum independent
	// observation interval. Missing confirmation must not wait for tomorrow's
	// cold sweep, nor reuse the current scope coverage as a second observation.
	due := now.Add(observationMissingInterval)
	return tx.Model(&models.ProviderObservationScope{}).Where("id = ? AND next_coverage_at > ?", item.ScopeID, due).Update("next_coverage_at", due).Error
}

// RepairPaths performs bounded local parent-graph updates after a directory
// rename or move. No per-file provider calls, recursive transaction or pruning.
func (s *ProviderObservationService) RepairPaths(ctx context.Context, scopeID string, limit int) (int, error) {
	if limit < 1 || limit > 250 {
		return 0, ErrObservationInvalid
	}
	changed := 0
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var scope models.ProviderObservationScope
		if err := tx.First(&scope, "id = ?", scopeID).Error; err != nil {
			return err
		}
		if err := s.validScopeTx(tx, scope); err != nil {
			return err
		}
		var parent models.ProviderObservationItem
		err := observationCurrent(tx, scopeID).Where("path_repair_pending = ? AND is_dir = ?", true, true).Order("relative_path").First(&parent).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		var rows []models.ProviderObservationItem
		if err = observationCurrent(tx, scopeID).Where("parent_id = ? AND resource_id > ?", parent.ResourceID, parent.PathRepairCursor).Order("resource_id").Limit(limit).Find(&rows).Error; err != nil {
			return err
		}
		now := s.now()
		batch := models.ProviderObservationBatch{ID: uuid.NewString(), ScopeID: scopeID, Fence: scope.Fence, DirectoryID: parent.ResourceID, CoverageRound: scope.CoverageRound, RuntimeID: s.runtimeID, State: "local_paths", StartedAt: now, CompletedAt: &now, UpdatedAt: now}
		for i := range rows {
			row := &rows[i]
			parent.PathRepairCursor = row.ResourceID
			next := path.Join(parent.RelativePath, row.Name)
			if row.RelativePath == next {
				continue
			}
			old := row.RelativePath
			row.RelativePath = next
			row.UpdatedAt = now
			if row.IsDir {
				row.PathRepairPending = true
				row.PathRepairCursor = ""
				row.DueAt = now
			}
			if err = observationUpsert(tx, row); err != nil {
				return err
			}
			if !row.Excluded && row.DownlistedAt == nil {
				fact := *row
				fact.BatchID = batch.ID
				fact.Change = "moved"
				fact.PreviousPath = old
				batch.Revision++
				fact.Revision = batch.Revision
				if err = observationUpsert(tx, &fact); err != nil {
					return err
				}
				changed++
			}
		}
		if len(rows) < limit {
			parent.PathRepairPending = false
			parent.PathRepairCursor = ""
		}
		if err = observationUpsert(tx, &parent); err != nil {
			return err
		}
		if batch.Revision > 0 {
			return tx.Create(&batch).Error
		}
		return nil
	})
	return changed, err
}
