package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	storagefs "github.com/yuanjing-hash/OhMyCine-Server/internal/storage"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/nfo"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const catalogArtifactLocalWorkers = 4

type catalogArtifactScopedWork struct {
	item     models.CatalogArtifactBindingItem
	key      string
	safePath string
	execute  func(context.Context) (string, error)
}

type catalogArtifactScopedResult struct {
	item     models.CatalogArtifactBindingItem
	outcome  string
	err      error
	safePath string
}

const catalogArtifactItemMaxAttempts = 4

func artifactItemFailure(err error) (string, string, bool) {
	if err == nil {
		return "", "", false
	}
	if code, retryable := cloudpkg.ErrorInfo(err); code != "" {
		switch code {
		case cloudpkg.CodeAuthExpired, cloudpkg.CodeCookieInvalid:
			return code, "云盘登录凭据已失效", false
		default:
			return code, "云盘暂时无法完成该文件", retryable
		}
	}
	text := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, os.ErrPermission):
		return "artifact_permission_denied", "没有权限写入目标目录", false
	case strings.Contains(text, "escape") || strings.Contains(text, "symlink") || strings.Contains(text, "invalid") || strings.Contains(text, "conflict"):
		return "artifact_path_invalid", "目标路径不安全或与现有文件冲突", false
	case errors.Is(err, context.DeadlineExceeded):
		return "artifact_io_timeout", "文件操作超时", true
	default:
		return "artifact_io_failed", "文件暂时无法写入", true
	}
}

func safeArtifactRelativePath(value string) string {
	value = filepath.ToSlash(strings.TrimSpace(value))
	if value == "" || filepath.IsAbs(filepath.FromSlash(value)) || filepath.VolumeName(filepath.FromSlash(value)) != "" {
		return ""
	}
	value = strings.TrimPrefix(value, "/")
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return ""
	}
	return "/" + clean
}

type catalogArtifactInspection struct {
	healthy     bool
	artifactIDs []uint
}

func catalogArtifactFatalError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrCatalogFence) || ErrorCode(err) == CodeQueueLeaseInvalid {
		return true
	}
	code, _ := cloudpkg.ErrorInfo(err)
	return code == cloudpkg.CodeAuthExpired || code == cloudpkg.CodeCookieInvalid
}

// Scope preparation is intentionally read-only, so bounded hashing can run in
// parallel without multiplying SQLite writers. The service-wide slots keep two
// libraries from turning one disk into an unbounded random-I/O workload.
func (s *MediaArtifactService) inspectCatalogArtifactHealth(ctx context.Context, count int, inspect func(int) catalogArtifactInspection) ([]catalogArtifactInspection, error) {
	results := make([]catalogArtifactInspection, count)
	if count == 0 {
		return results, nil
	}
	workers := min(catalogArtifactLocalWorkers, count)
	indices := make(chan int)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range indices {
				if ctx.Err() != nil {
					continue
				}
				if s.localSlots != nil {
					select {
					case s.localSlots <- struct{}{}:
					case <-ctx.Done():
						continue
					}
				}
				results[index] = inspect(index)
				if s.localSlots != nil {
					<-s.localSlots
				}
			}
		}()
	}
	for index := 0; index < count; index++ {
		select {
		case indices <- index:
		case <-ctx.Done():
			close(indices)
			group.Wait()
			return nil, ctx.Err()
		}
	}
	close(indices)
	group.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func (s *MediaArtifactService) startCatalogArtifactItems(ctx context.Context, policy mediaArtifactPolicy, run models.MediaArtifactRun, claim ClaimedJob, items []models.CatalogArtifactBindingItem) error {
	if len(items) == 0 {
		return nil
	}
	return s.catalogArtifactWriteTx(ctx, func(tx *gorm.DB) error {
		if err := s.validateArtifactBindingTx(tx, policy, &run, &claim, true); err != nil {
			return err
		}
		now := time.Now().UTC()
		byKind := make(map[string][]uint)
		for _, item := range items {
			byKind[item.EntityKind] = append(byKind[item.EntityKind], item.EntityID)
		}
		for kind, ids := range byKind {
			if err := tx.Model(&models.CatalogArtifactBindingItem{}).Where("binding_id = ? AND entity_kind = ? AND entity_id IN ? AND status <> ?", items[0].BindingID, kind, ids, "completed").Updates(map[string]any{"status": "running", "attempts": gorm.Expr("attempts + 1"), "outcome": "", "error_code": "", "updated_at": now}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *MediaArtifactService) finishCatalogArtifactItems(ctx context.Context, policy mediaArtifactPolicy, run models.MediaArtifactRun, claim ClaimedJob, results []catalogArtifactScopedResult) error {
	if len(results) == 0 {
		return nil
	}
	return s.catalogArtifactWriteTx(ctx, func(tx *gorm.DB) error {
		if err := s.validateArtifactBindingTx(tx, policy, &run, &claim, true); err != nil {
			return err
		}
		now := time.Now().UTC()
		for _, result := range results {
			status, outcome, code, message, retryable := "completed", result.outcome, "", "", false
			credentialWait := false
			if outcome == "" {
				outcome = "noop"
			}
			if result.err != nil {
				status, outcome = "failed", ""
				code, message, retryable = artifactItemFailure(result.err)
				credentialWait = code == cloudpkg.CodeAuthExpired || code == cloudpkg.CodeCookieInvalid
				if credentialWait {
					// Expired credentials are a library readiness wait, not a failed
					// file attempt. Leave the exact item pending so renewal resumes it
					// without exhausting either item or queue failure budgets.
					status, retryable = "pending", true
				}
			}
			updates := map[string]any{"status": status, "outcome": outcome, "error_code": code, "error_message": message, "safe_relative_path": safeArtifactRelativePath(result.safePath), "retryable": retryable, "next_attempt_at": nil, "updated_at": now}
			if credentialWait {
				updates["attempts"] = gorm.Expr("CASE WHEN attempts > 0 THEN attempts - 1 ELSE 0 END")
				updates["finished_at"] = nil
			} else if status == "failed" {
				updates["finished_at"] = now
				if retryable {
					delay := time.Minute * time.Duration(1<<min(result.item.Attempts, 5))
					updates["next_attempt_at"] = now.Add(delay)
				}
			} else {
				updates["finished_at"] = now
			}
			if err := tx.Model(&models.CatalogArtifactBindingItem{}).Where("binding_id = ? AND entity_kind = ? AND entity_id = ?", result.item.BindingID, result.item.EntityKind, result.item.EntityID).Updates(updates).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// Deterministic lanes keep the same source/target ordered while allowing
// independent local file writes to overlap. Provider-backed source assets are
// deliberately executed with one lane by the caller.
func (s *MediaArtifactService) executeCatalogArtifactWork(ctx context.Context, work []catalogArtifactScopedWork, workers int, consume func(catalogArtifactScopedResult) error) error {
	if len(work) == 0 {
		return nil
	}
	executionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	workers = min(max(workers, 1), min(catalogArtifactLocalWorkers, len(work)))
	lanes := make([]chan catalogArtifactScopedWork, workers)
	results := make(chan catalogArtifactScopedResult, workers)
	var group sync.WaitGroup
	for i := range lanes {
		lanes[i] = make(chan catalogArtifactScopedWork)
		group.Add(1)
		go func(input <-chan catalogArtifactScopedWork) {
			defer group.Done()
			for item := range input {
				if err := executionCtx.Err(); err != nil {
					results <- catalogArtifactScopedResult{item: item.item, err: err}
					continue
				}
				if s.localSlots != nil {
					select {
					case s.localSlots <- struct{}{}:
					case <-executionCtx.Done():
						results <- catalogArtifactScopedResult{item: item.item, err: executionCtx.Err()}
						continue
					}
				}
				outcome, err := item.execute(executionCtx)
				if s.localSlots != nil {
					<-s.localSlots
				}
				// Stop dispatch at the execution boundary instead of waiting for the
				// result consumer. A one-lane Provider batch can otherwise run several
				// more credential-guard failures before the consumer is scheduled.
				if catalogArtifactFatalError(err) {
					cancel()
				}
				results <- catalogArtifactScopedResult{item: item.item, outcome: outcome, err: err, safePath: item.safePath}
			}
		}(lanes[i])
	}
	go func() {
		defer func() {
			for _, lane := range lanes {
				close(lane)
			}
		}()
		for _, item := range work {
			hash := fnv.New32a()
			_, _ = hash.Write([]byte(item.key))
			select {
			case lanes[int(hash.Sum32()%uint32(workers))] <- item:
			case <-executionCtx.Done():
				return
			}
		}
	}()
	go func() {
		group.Wait()
		close(results)
	}()
	var consumeErr error
	for result := range results {
		if consumeErr == nil {
			consumeErr = consume(result)
			if consumeErr != nil {
				cancel()
			}
		}
	}
	return consumeErr
}

type catalogArtifactManifestSet struct {
	bySource      map[string][]models.MediaArtifact
	byRecognition map[uint][]models.MediaArtifact
}

type catalogArtifactRebind struct {
	ID             uint
	ProviderItemID string
	UpdateProvider bool
}

func (s *MediaArtifactService) loadCatalogArtifactManifest(ctx context.Context, run models.MediaArtifactRun, policy mediaArtifactPolicy) (catalogArtifactManifestSet, error) {
	result := catalogArtifactManifestSet{bySource: map[string][]models.MediaArtifact{}, byRecognition: map[uint][]models.MediaArtifact{}}
	for after := uint(0); ; {
		var rows []models.MediaArtifact
		if err := s.catalogStore.readDB.WithContext(ctx).
			Where("library_id = ? AND target_kind = ? AND active = ? AND id > ?", run.LibraryID, policy.TargetKind, true, after).
			Order("id").Limit(CatalogBatchRows).Find(&rows).Error; err != nil {
			return result, err
		}
		if len(rows) == 0 {
			return result, nil
		}
		for _, artifact := range rows {
			result.bySource[artifact.SourceIdentity] = append(result.bySource[artifact.SourceIdentity], artifact)
			if recognitionID, ok := catalogArtifactRecognitionSourceID(artifact.SourceIdentity); ok {
				result.byRecognition[recognitionID] = append(result.byRecognition[recognitionID], artifact)
			}
		}
		after = rows[len(rows)-1].ID
	}
}

func catalogArtifactRecognitionSourceID(source string) (uint, bool) {
	const prefix = "recognition:"
	if !strings.HasPrefix(source, prefix) {
		return 0, false
	}
	raw := strings.TrimPrefix(source, prefix)
	if index := strings.IndexByte(raw, ':'); index >= 0 {
		raw = raw[:index]
	}
	parsed, err := strconv.ParseUint(raw, 10, 64)
	return uint(parsed), err == nil && parsed > 0 && uint64(uint(parsed)) == parsed
}

func (s *MediaArtifactService) catalogSTRMArtifactsHealthy(root string, entry models.MediaLibraryEntry, rows []models.MediaArtifact, verifier signedArtifactVerifier) (bool, []uint) {
	relative, err := strmRelativePath(entry.RelativePath)
	if err != nil {
		return false, nil
	}
	if len(rows) != 1 || rows[0].RelativePath != relative || !rows[0].Managed || rows[0].Status != models.MediaArtifactStatusCompleted {
		return false, nil
	}
	target, err := storagefs.Constrain(root, filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(relative, "/"))))
	if err != nil {
		return false, nil
	}
	content, inspection, ok := s.reusableSTRM(target, rows[0], verifier)
	if !ok {
		return false, nil
	}
	fingerprint := sha256.Sum256(content)
	if rows[0].ContentFingerprint != hex.EncodeToString(fingerprint[:]) {
		return false, nil
	}
	_ = inspection
	return true, []uint{rows[0].ID}
}

func artifactFileMatches(root string, artifact models.MediaArtifact) bool {
	target, err := storagefs.Constrain(root, filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(artifact.RelativePath, "/"))))
	if err != nil {
		return false
	}
	info, err := os.Lstat(target)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 || info.Size() > 32<<20 {
		return false
	}
	body, err := os.ReadFile(target)
	if err != nil {
		return false
	}
	digest := sha256.Sum256(body)
	return artifact.ContentFingerprint != "" && artifact.ContentFingerprint == hex.EncodeToString(digest[:])
}

func (s *MediaArtifactService) commitCatalogArtifactScopePage(ctx context.Context, policy mediaArtifactPolicy, run models.MediaArtifactRun, claim ClaimedJob, row models.CatalogArtifactBinding, kind string, cursor uint, pending []models.CatalogArtifactBindingItem, noops []uint, rebind []catalogArtifactRebind) error {
	cursorColumn := ""
	switch kind {
	case "entry":
		cursorColumn = "scope_entry_after_id"
	case "asset":
		cursorColumn = "scope_asset_after_id"
	case "recognition":
		cursorColumn = "scope_recognition_after_id"
	default:
		return ErrCatalogInvalid
	}
	return s.catalogArtifactWriteTx(ctx, func(tx *gorm.DB) error {
		if err := s.validateArtifactBindingTx(tx, policy, &run, &claim, true); err != nil {
			return err
		}
		now := time.Now().UTC()
		rebindIDs := make([]uint, 0, len(rebind))
		for _, artifact := range rebind {
			rebindIDs = append(rebindIDs, artifact.ID)
		}
		for start := 0; start < len(rebindIDs); start += CatalogBatchRows {
			end := min(start+CatalogBatchRows, len(rebind))
			if err := tx.Model(&models.MediaArtifact{}).
				Where("id IN ? AND library_id = ? AND target_kind = ? AND active = ? AND managed = ? AND status = ?", rebindIDs[start:end], run.LibraryID, policy.TargetKind, true, true, models.MediaArtifactStatusCompleted).
				Updates(map[string]any{"run_id": run.ID, "catalog_binding_id": row.ID, "updated_at": now}).Error; err != nil {
				return err
			}
		}
		for _, artifact := range rebind {
			if !artifact.UpdateProvider {
				continue
			}
			if err := tx.Model(&models.MediaArtifact{}).Where("id = ? AND provider_item_id <> ?", artifact.ID, artifact.ProviderItemID).Update("provider_item_id", artifact.ProviderItemID).Error; err != nil {
				return err
			}
		}
		if len(pending) > 0 {
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(pending, CatalogBatchRows).Error; err != nil {
				return err
			}
		}
		for start := 0; start < len(noops); start += CatalogBatchRows {
			end := min(start+CatalogBatchRows, len(noops))
			if err := tx.Model(&models.CatalogArtifactBindingItem{}).
				Where("binding_id = ? AND entity_kind = ? AND entity_id IN ?", row.ID, kind, noops[start:end]).
				Updates(map[string]any{"status": "completed", "outcome": "noop", "error_code": "", "updated_at": now}).Error; err != nil {
				return err
			}
		}
		return tx.Model(&models.CatalogArtifactBinding{}).Where("id = ? AND scope_prepared = ?", row.ID, false).
			Updates(map[string]any{cursorColumn: cursor, "updated_at": now}).Error
	})
}

// Both publication-time deltas and explicit full audits converge through the
// same durable item table. Full mode reads the complete desired tree, but it
// emits only missing/stale/orphan differences. The three cursors make that
// comparison resumable without deleting already prepared work after a crash.
func (s *MediaArtifactService) prepareCatalogArtifactScope(ctx context.Context, runtime JobRuntime, root string, claim ClaimedJob, run models.MediaArtifactRun, policy mediaArtifactPolicy, row models.CatalogArtifactBinding, binding CatalogSnapshotBinding, verifier signedArtifactVerifier) error {
	if row.ScopePrepared {
		return nil
	}
	manifest, err := s.loadCatalogArtifactManifest(ctx, run, policy)
	if err != nil {
		return err
	}
	read := func(fn func(*CatalogReader) error) error {
		return s.catalogStore.ReadBoundCatalog(ctx, binding, "artifact", row.ID, func(reader *CatalogReader) error {
			if err := s.validateArtifactBindingTx(reader.tx, policy, &run, &claim, false); err != nil {
				return err
			}
			return fn(reader)
		})
	}
	heartbeat := func() error {
		if runtime == nil {
			return nil
		}
		// Preparation has not yet established the physical difference count, so
		// preserve unknown progress while proving liveness once per bounded page.
		return runtime.Heartbeat(nil, nil, nil, nil, nil)
	}
	prepareEntries := func() error {
		for after := row.ScopeEntryAfterID; ; {
			var sourceItems []models.CatalogArtifactBindingItem
			if row.ScopeMode == catalogArtifactScopeIncremental {
				if err := s.catalogStore.readDB.WithContext(ctx).Where("binding_id = ? AND entity_kind = ? AND entity_id > ?", row.ID, "entry", after).Order("entity_id").Limit(CatalogBatchRows).Find(&sourceItems).Error; err != nil {
					return err
				}
				if len(sourceItems) == 0 {
					return nil
				}
			}
			var entries []models.MediaLibraryEntry
			if err := read(func(r *CatalogReader) error {
				query := r.Entries().Where("library_id = ? AND id > ?", run.LibraryID, after)
				if row.ScopeMode == catalogArtifactScopeIncremental {
					ids := make([]uint, 0, len(sourceItems))
					for _, item := range sourceItems {
						ids = append(ids, item.EntityID)
					}
					query = r.Entries().Where("library_id = ? AND id IN ?", run.LibraryID, ids)
				}
				return query.Order("id").Limit(CatalogBatchRows).Find(&entries).Error
			}); err != nil {
				return err
			}
			if row.ScopeMode == catalogArtifactScopeFull && len(entries) == 0 {
				return nil
			}
			byID := make(map[uint]models.MediaLibraryEntry, len(entries))
			for _, entry := range entries {
				byID[entry.ID] = entry
			}
			ids := make([]uint, 0, len(entries))
			if row.ScopeMode == catalogArtifactScopeIncremental {
				for _, item := range sourceItems {
					ids = append(ids, item.EntityID)
				}
			} else {
				for _, entry := range entries {
					ids = append(ids, entry.ID)
				}
			}
			inspections, err := s.inspectCatalogArtifactHealth(ctx, len(ids), func(index int) catalogArtifactInspection {
				id := ids[index]
				entry, exists := byID[id]
				artifacts := manifest.bySource[fmt.Sprintf("entry:%d", id)]
				applicable := policy.STRMEnabled
				if exists && applicable {
					healthy, artifactIDs := s.catalogSTRMArtifactsHealthy(root, entry, artifacts, verifier)
					return catalogArtifactInspection{healthy: healthy, artifactIDs: artifactIDs}
				}
				return catalogArtifactInspection{}
			})
			if err != nil {
				return err
			}
			pending, noops, rebind := make([]models.CatalogArtifactBindingItem, 0), make([]uint, 0), make([]catalogArtifactRebind, 0)
			for index, id := range ids {
				entry, exists := byID[id]
				artifacts := manifest.bySource[fmt.Sprintf("entry:%d", id)]
				applicable := policy.STRMEnabled
				healthy, artifactIDs := inspections[index].healthy, inspections[index].artifactIDs
				if healthy {
					for _, artifactID := range artifactIDs {
						rebind = append(rebind, catalogArtifactRebind{ID: artifactID, ProviderItemID: entry.ProviderID, UpdateProvider: artifacts[0].ProviderItemID != entry.ProviderID})
					}
					if row.ScopeMode == catalogArtifactScopeIncremental {
						noops = append(noops, id)
					}
				} else if row.ScopeMode == catalogArtifactScopeIncremental {
					if (!applicable || !exists) && len(artifacts) == 0 {
						noops = append(noops, id)
					} else {
						pending = append(pending, models.CatalogArtifactBindingItem{BindingID: row.ID, EntityKind: "entry", EntityID: id, Status: "pending", UpdatedAt: time.Now().UTC()})
					}
				} else if applicable {
					pending = append(pending, models.CatalogArtifactBindingItem{BindingID: row.ID, EntityKind: "entry", EntityID: id, Status: "pending", UpdatedAt: time.Now().UTC()})
				}
			}
			after = ids[len(ids)-1]
			if err := s.commitCatalogArtifactScopePage(ctx, policy, run, claim, row, "entry", after, pending, noops, rebind); err != nil {
				return err
			}
			if err := heartbeat(); err != nil {
				return err
			}
		}
	}
	if err := prepareEntries(); err != nil {
		return err
	}
	prepareAssets := func() error {
		for after := row.ScopeAssetAfterID; ; {
			var sourceItems []models.CatalogArtifactBindingItem
			if row.ScopeMode == catalogArtifactScopeIncremental {
				if err := s.catalogStore.readDB.WithContext(ctx).Where("binding_id = ? AND entity_kind = ? AND entity_id > ?", row.ID, "asset", after).Order("entity_id").Limit(CatalogBatchRows).Find(&sourceItems).Error; err != nil {
					return err
				}
				if len(sourceItems) == 0 {
					return nil
				}
			}
			var assets []models.MediaLibrarySourceAsset
			if err := read(func(r *CatalogReader) error {
				query := r.SourceAssets().Where("library_id = ? AND active = ? AND id > ?", run.LibraryID, true, after)
				if row.ScopeMode == catalogArtifactScopeIncremental {
					ids := make([]uint, 0, len(sourceItems))
					for _, item := range sourceItems {
						ids = append(ids, item.EntityID)
					}
					query = r.SourceAssets().Where("library_id = ? AND active = ? AND id IN ?", run.LibraryID, true, ids)
				}
				return query.Order("id").Limit(CatalogBatchRows).Find(&assets).Error
			}); err != nil {
				return err
			}
			if row.ScopeMode == catalogArtifactScopeFull && len(assets) == 0 {
				return nil
			}
			byID := make(map[uint]models.MediaLibrarySourceAsset, len(assets))
			for _, asset := range assets {
				byID[asset.ID] = asset
			}
			ids := make([]uint, 0, len(assets))
			if row.ScopeMode == catalogArtifactScopeIncremental {
				for _, item := range sourceItems {
					ids = append(ids, item.EntityID)
				}
			} else {
				for _, asset := range assets {
					ids = append(ids, asset.ID)
				}
			}
			inspections, err := s.inspectCatalogArtifactHealth(ctx, len(ids), func(index int) catalogArtifactInspection {
				id := ids[index]
				asset, exists := byID[id]
				artifacts := manifest.bySource[fmt.Sprintf("asset:%d", id)]
				applicable := policy.STRMEnabled && policy.StorageType != models.StorageTypeLocal
				healthy := exists && applicable && len(artifacts) == 1 && artifacts[0].RelativePath == asset.RelativePath && artifacts[0].Managed && artifacts[0].Status == models.MediaArtifactStatusCompleted && artifacts[0].SourceFingerprint == artifactAssetSourceFingerprint(asset) && artifactFileMatches(root, artifacts[0])
				ids := []uint(nil)
				if healthy {
					ids = []uint{artifacts[0].ID}
				}
				return catalogArtifactInspection{healthy: healthy, artifactIDs: ids}
			})
			if err != nil {
				return err
			}
			pending, noops, rebind := make([]models.CatalogArtifactBindingItem, 0), make([]uint, 0), make([]catalogArtifactRebind, 0)
			for index, id := range ids {
				asset, exists := byID[id]
				artifacts := manifest.bySource[fmt.Sprintf("asset:%d", id)]
				applicable := policy.STRMEnabled && policy.StorageType != models.StorageTypeLocal
				healthy := inspections[index].healthy
				if healthy {
					rebind = append(rebind, catalogArtifactRebind{ID: inspections[index].artifactIDs[0], ProviderItemID: asset.ProviderID, UpdateProvider: artifacts[0].ProviderItemID != asset.ProviderID})
					if row.ScopeMode == catalogArtifactScopeIncremental {
						noops = append(noops, id)
					}
				} else if row.ScopeMode == catalogArtifactScopeIncremental {
					if (!applicable || !exists) && len(artifacts) == 0 {
						noops = append(noops, id)
					} else {
						pending = append(pending, models.CatalogArtifactBindingItem{BindingID: row.ID, EntityKind: "asset", EntityID: id, Status: "pending", UpdatedAt: time.Now().UTC()})
					}
				} else if applicable {
					pending = append(pending, models.CatalogArtifactBindingItem{BindingID: row.ID, EntityKind: "asset", EntityID: id, Status: "pending", UpdatedAt: time.Now().UTC()})
				}
			}
			after = ids[len(ids)-1]
			if err := s.commitCatalogArtifactScopePage(ctx, policy, run, claim, row, "asset", after, pending, noops, rebind); err != nil {
				return err
			}
			if err := heartbeat(); err != nil {
				return err
			}
		}
	}
	if err := prepareAssets(); err != nil {
		return err
	}
	prepareRecognitions := func() error {
		for after := row.ScopeRecognitionAfterID; ; {
			var sourceItems []models.CatalogArtifactBindingItem
			if row.ScopeMode == catalogArtifactScopeIncremental {
				if err := s.catalogStore.readDB.WithContext(ctx).Where("binding_id = ? AND entity_kind = ? AND entity_id > ?", row.ID, "recognition", after).Order("entity_id").Limit(25).Find(&sourceItems).Error; err != nil {
					return err
				}
				if len(sourceItems) == 0 {
					return nil
				}
			}
			var records []models.MediaLibraryRecognition
			if err := read(func(r *CatalogReader) error {
				query := r.Recognitions().Where("library_id = ? AND status = ? AND id > ?", run.LibraryID, mediaRecognitionStatusMatched, after)
				if row.ScopeMode == catalogArtifactScopeIncremental {
					ids := make([]uint, 0, len(sourceItems))
					for _, item := range sourceItems {
						ids = append(ids, item.EntityID)
					}
					query = r.Recognitions().Where("library_id = ? AND status = ? AND id IN ?", run.LibraryID, mediaRecognitionStatusMatched, ids)
				}
				return query.Order("id").Limit(25).Find(&records).Error
			}); err != nil {
				return err
			}
			if row.ScopeMode == catalogArtifactScopeFull && len(records) == 0 {
				return nil
			}
			byID := make(map[uint]models.MediaLibraryRecognition, len(records))
			ids := make([]uint, 0, max(len(records), len(sourceItems)))
			for _, record := range records {
				byID[record.ID], ids = record, append(ids, record.ID)
			}
			if row.ScopeMode == catalogArtifactScopeIncremental {
				ids = ids[:0]
				for _, item := range sourceItems {
					ids = append(ids, item.EntityID)
				}
			}
			var entries []models.MediaLibraryEntry
			if len(ids) > 0 {
				if err := read(func(r *CatalogReader) error {
					return r.Entries().Where("library_id = ? AND recognition_id IN ?", run.LibraryID, ids).Order("recognition_id,relative_path").Find(&entries).Error
				}); err != nil {
					return err
				}
			}
			entriesByRecognition := make(map[uint][]models.MediaLibraryEntry, len(records))
			for _, entry := range entries {
				if entry.RecognitionID != nil {
					entriesByRecognition[*entry.RecognitionID] = append(entriesByRecognition[*entry.RecognitionID], entry)
				}
			}
			inspections, err := s.inspectCatalogArtifactHealth(ctx, len(ids), func(index int) catalogArtifactInspection {
				id := ids[index]
				record, exists := byID[id]
				artifacts := manifest.byRecognition[id]
				applicable := policy.Metadata
				if exists && applicable {
					healthy, artifactIDs := s.fullRecognitionArtifactsHealthy(root, record, entriesByRecognition[id], artifacts)
					return catalogArtifactInspection{healthy: healthy, artifactIDs: artifactIDs}
				}
				return catalogArtifactInspection{}
			})
			if err != nil {
				return err
			}
			pending, noops, rebind := make([]models.CatalogArtifactBindingItem, 0), make([]uint, 0), make([]catalogArtifactRebind, 0)
			for index, id := range ids {
				_, exists := byID[id]
				artifacts := manifest.byRecognition[id]
				applicable := policy.Metadata
				healthy, artifactIDs := inspections[index].healthy, inspections[index].artifactIDs
				if healthy {
					for _, artifactID := range artifactIDs {
						rebind = append(rebind, catalogArtifactRebind{ID: artifactID})
					}
					if row.ScopeMode == catalogArtifactScopeIncremental {
						noops = append(noops, id)
					}
				} else if row.ScopeMode == catalogArtifactScopeIncremental {
					if (!applicable || !exists) && len(artifacts) == 0 {
						noops = append(noops, id)
					} else {
						pending = append(pending, models.CatalogArtifactBindingItem{BindingID: row.ID, EntityKind: "recognition", EntityID: id, Status: "pending", UpdatedAt: time.Now().UTC()})
					}
				} else if applicable {
					pending = append(pending, models.CatalogArtifactBindingItem{BindingID: row.ID, EntityKind: "recognition", EntityID: id, Status: "pending", UpdatedAt: time.Now().UTC()})
				}
			}
			after = ids[len(ids)-1]
			if err := s.commitCatalogArtifactScopePage(ctx, policy, run, claim, row, "recognition", after, pending, noops, rebind); err != nil {
				return err
			}
			if err := heartbeat(); err != nil {
				return err
			}
		}
	}
	if err := prepareRecognitions(); err != nil {
		return err
	}
	if row.ScopeMode == catalogArtifactScopeFull && policy.CleanupEligible {
		for after := uint(0); ; {
			var artifacts []models.MediaArtifact
			if err := s.catalogStore.readDB.WithContext(ctx).Where(`media_artifacts.library_id = ? AND media_artifacts.target_kind = ? AND media_artifacts.active = ? AND media_artifacts.id > ? AND media_artifacts.catalog_binding_id <> ? AND NOT EXISTS (
				SELECT 1 FROM catalog_artifact_binding_items work WHERE work.binding_id = ? AND (
				 (work.entity_kind = 'entry' AND media_artifacts.source_identity = 'entry:' || work.entity_id) OR
				 (work.entity_kind = 'asset' AND media_artifacts.source_identity = 'asset:' || work.entity_id) OR
				 (work.entity_kind = 'recognition' AND (media_artifacts.source_identity = 'recognition:' || work.entity_id OR media_artifacts.source_identity LIKE 'recognition:' || work.entity_id || ':%'))
				)
			)`, run.LibraryID, policy.TargetKind, true, after, row.ID, row.ID).Order("media_artifacts.id").Limit(CatalogBatchRows).Find(&artifacts).Error; err != nil {
				return err
			}
			if len(artifacts) == 0 {
				break
			}
			items := make([]models.CatalogArtifactBindingItem, 0, len(artifacts))
			for _, artifact := range artifacts {
				items = append(items, models.CatalogArtifactBindingItem{BindingID: row.ID, EntityKind: "manifest", EntityID: artifact.ID, Status: "pending", UpdatedAt: time.Now().UTC()})
			}
			if err := s.catalogArtifactWriteTx(ctx, func(tx *gorm.DB) error {
				if err := s.validateArtifactBindingTx(tx, policy, &run, &claim, true); err != nil {
					return err
				}
				return tx.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(items, CatalogBatchRows).Error
			}); err != nil {
				return err
			}
			if err := heartbeat(); err != nil {
				return err
			}
			after = artifacts[len(artifacts)-1].ID
		}
	}
	return s.catalogArtifactWriteTx(ctx, func(tx *gorm.DB) error {
		if err := s.validateArtifactBindingTx(tx, policy, &run, &claim, true); err != nil {
			return err
		}
		return tx.Model(&models.CatalogArtifactBinding{}).Where("id = ? AND scope_prepared = ?", row.ID, false).Updates(map[string]any{"scope_prepared": true, "updated_at": time.Now().UTC()}).Error
	})
}

func (s *MediaArtifactService) fullRecognitionArtifactsHealthy(root string, record models.MediaLibraryRecognition, entries []models.MediaLibraryEntry, rows []models.MediaArtifact) (bool, []uint) {
	if len(entries) == 0 {
		return false, nil
	}
	_, snapshot, err := decodeRecognitionMetadata(record.MetadataJSON)
	if err != nil {
		return false, nil
	}
	nfoBody, err := nfo.Render(snapshot)
	if err != nil {
		return false, nil
	}
	nfoRelative, err := nfoRelativePath(snapshot.MediaType, entries)
	if err != nil {
		return false, nil
	}
	expected := map[string]string{fmt.Sprintf("recognition:%d", record.ID): nfoRelative}
	for _, image := range nfo.Images(snapshot) {
		relative, _, err := nfoImageRelativePath(nfoRelative, image)
		if err != nil {
			return false, nil
		}
		season := ""
		if image.SeasonNumber != nil {
			season = fmt.Sprintf(":%d", *image.SeasonNumber)
		}
		expected[fmt.Sprintf("recognition:%d:%s%s", record.ID, image.Kind, season)] = relative
	}
	if len(rows) != len(expected) {
		return false, nil
	}
	nfoDigest := sha256.Sum256(nfoBody)
	ids := make([]uint, 0, len(rows))
	for _, artifact := range rows {
		if expected[artifact.SourceIdentity] != artifact.RelativePath || !artifact.Managed || artifact.Status != models.MediaArtifactStatusCompleted || !artifactFileMatches(root, artifact) {
			return false, nil
		}
		if artifact.SourceIdentity == fmt.Sprintf("recognition:%d", record.ID) && artifact.ContentFingerprint != hex.EncodeToString(nfoDigest[:]) {
			return false, nil
		}
		ids = append(ids, artifact.ID)
	}
	return true, ids
}

// The source snapshot is immutable; each read page and manifest writer is
// bounded. No catalog transaction spans rendering, downloads or physical I/O.
func (s *MediaArtifactService) generateBoundArtifacts(ctx context.Context, runtime JobRuntime, claim ClaimedJob, permit CatalogPhysicalWritePermit, run models.MediaArtifactRun, policy mediaArtifactPolicy) WorkerResult {
	fail := func(err error) WorkerResult {
		code := "artifact_snapshot_failed"
		if errors.Is(err, ErrCatalogFence) {
			code = "artifact_snapshot_changed"
		}
		if cloudCode, _ := cloudpkg.ErrorInfo(err); cloudCode == cloudpkg.CodeAuthExpired || cloudCode == cloudpkg.CodeCookieInvalid {
			code = cloudCode
		}
		_ = s.catalogArtifactWriteTx(ctx, func(tx *gorm.DB) error {
			if s.queue == nil {
				return ErrCatalogInvalid
			}
			if _, e := s.queue.verifyLease(tx, claim.Job.ID, claim.LeaseToken); e != nil {
				return e
			}
			var current models.MediaArtifactRun
			if e := tx.First(&current, "id = ?", run.ID).Error; e != nil {
				return e
			}
			if current.PolicyJSON != run.PolicyJSON {
				return nil
			}
			now := time.Now().UTC()
			if e := tx.Model(&models.MediaArtifactRun{}).Where("id = ?", run.ID).Updates(map[string]any{"status": models.MediaArtifactStatusFailed, "error_code": code, "updated_at": now}).Error; e != nil {
				return e
			}
			if e := tx.Model(&models.MediaLibrary{}).Where("id = ? AND artifact_generation = ?", run.LibraryID, run.Generation).Updates(map[string]any{"artifact_status": models.MediaArtifactStatusFailed, "artifact_error": code, "artifact_updated_at": now}).Error; e != nil {
				return e
			}
			return tx.Model(&models.CatalogArtifactBinding{}).Where("id = ? AND state NOT IN ?", policy.CatalogBindingID, []string{"applying", "completed", "superseded"}).Updates(map[string]any{"state": "failed", "updated_at": now}).Error
		})
		next := time.Now().UTC().Add(time.Minute)
		message := "媒体产物快照已变化或生成失败，将重新校验后重试"
		if code == cloudpkg.CodeAuthExpired || code == cloudpkg.CodeCookieInvalid {
			// Library readiness prevents another claim while the connection remains
			// offline. Keeping the durable binding/items allows credential renewal to
			// continue from the first unfinished source asset without replaying success.
			next = time.Now().UTC()
			message = "云盘登录凭据已失效，已保存进度并等待更新凭据"
		}
		return WorkerResult{RetryAt: &next, ErrorCode: code, ErrorMessage: message}
	}
	if s.catalogStore == nil {
		return fail(ErrCatalogInvalid)
	}
	var row models.CatalogArtifactBinding
	if err := s.catalogStore.readDB.WithContext(ctx).First(&row, "id = ?", policy.CatalogBindingID).Error; err != nil {
		return fail(err)
	}
	binding, err := artifactCatalogSnapshot(row)
	if err != nil {
		return fail(err)
	}
	check := func(checkCtx context.Context) error {
		if err := checkCtx.Err(); err != nil {
			return err
		}
		// Resolve filesystem identity outside the catalog transaction, including
		// after upstream downloads and immediately before each physical write.
		if policy.TargetKind != artifactTargetCloudCleanup {
			_, identity, err := canonicalProjectionRoot(policy.ProjectionRoot)
			if err != nil || identity != policy.ProjectionRootIdentity {
				return ErrCatalogFence
			}
		}
		return s.catalogStore.readDB.WithContext(checkCtx).Transaction(func(tx *gorm.DB) error { return s.validateArtifactBindingTx(tx, policy, &run, &claim, true) })
	}
	if err := check(ctx); err != nil {
		return fail(err)
	}
	if row.State == "applying" {
		return s.finalizeBoundArtifacts(ctx, claim, run, policy, binding, fail)
	}
	root := ""
	if policy.TargetKind != artifactTargetCloudCleanup {
		root, err = (storagefs.LocalDriver{}).CanonicalizeRoot(policy.ProjectionRoot)
		if err != nil {
			return fail(err)
		}
	}
	if superseded, err := s.artifactPolicySuperseded(policy); err != nil || superseded {
		return fail(ErrCatalogFence)
	}
	verifier := signedArtifactVerifier{}
	if policy.STRMEnabled {
		if s.signedProxy == nil {
			return fail(ErrCatalogInvalid)
		}
		verifier, err = s.signedProxy.activeSigningVerifier()
		if err != nil {
			return fail(err)
		}
	}
	if err := s.catalogArtifactWriteTx(ctx, func(tx *gorm.DB) error {
		if err := s.validateArtifactBindingTx(tx, policy, &run, &claim, true); err != nil {
			return err
		}
		return tx.Model(&models.CatalogArtifactBinding{}).Where("id = ?", row.ID).Updates(map[string]any{"state": "running", "updated_at": time.Now().UTC()}).Error
	}); err != nil {
		return fail(err)
	}
	if err := s.prepareCatalogArtifactScope(ctx, runtime, root, claim, run, policy, row, binding, verifier); err != nil {
		return fail(err)
	}
	if err := s.catalogStore.readDB.WithContext(ctx).First(&row, "id = ?", row.ID).Error; err != nil {
		return fail(err)
	}
	if !row.ScopePrepared {
		return fail(ErrCatalogInvalid)
	}
	run.ExpectedCount, run.WrittenCount, run.UpdatedCount, run.SkippedCount, run.FailedCount = 0, 0, 0, 0, 0
	eligible := `binding_id = ?`
	args := []any{row.ID}
	var total int64
	if err := s.catalogStore.readDB.WithContext(ctx).Model(&models.CatalogArtifactBindingItem{}).
		Where(eligible+" AND NOT (status = 'completed' AND outcome = 'noop')", args...).Count(&total).Error; err != nil {
		return fail(err)
	}
	type outcomeCount struct {
		Outcome string
		Count   int
	}
	var completed []outcomeCount
	if err := s.catalogStore.readDB.WithContext(ctx).Model(&models.CatalogArtifactBindingItem{}).
		Select("outcome, COUNT(*) AS count").Where(eligible+" AND status = 'completed' AND outcome <> 'noop'", args...).Group("outcome").Scan(&completed).Error; err != nil {
		return fail(err)
	}
	processedCount := 0
	for _, group := range completed {
		processedCount += group.Count
		switch group.Outcome {
		case "written":
			run.WrittenCount += group.Count
		case "updated":
			run.UpdatedCount += group.Count
		case "skipped":
			run.SkippedCount += group.Count
		}
	}
	run.ExpectedCount = int(total)
	heartbeat := func(processedDelta int) error {
		processedCount += processedDelta
		processed, total := int64(processedCount), int64(run.ExpectedCount)
		progress := float64(1)
		if total > 0 {
			progress = float64(processed) / float64(total)
		}
		return runtime.Heartbeat(&progress, &processed, &total, nil, nil)
	}
	if err := heartbeat(0); err != nil {
		return fail(err)
	}
	executeWrite := func(executionCtx context.Context, write func(*artifactManifestIndex) (string, error)) (string, error) {
		if err := check(executionCtx); err != nil {
			return "", err
		}
		manifest := newArtifactManifestIndex(8)
		manifest.physical = &artifactPhysicalExecution{ctx: executionCtx, permit: permit, policy: policy}
		manifest.catalogBindingID = policy.CatalogBindingID
		manifest.lazy = true
		manifest.beforeWrite = func() error { return check(executionCtx) }
		manifest.reserve = func(artifact *models.MediaArtifact) error {
			return s.catalogArtifactWriteTx(executionCtx, func(tx *gorm.DB) error {
				if err := s.validateArtifactBindingTx(tx, policy, &run, &claim, true); err != nil {
					return err
				}
				return tx.Create(artifact).Error
			})
		}
		outcome, err := write(manifest)
		if err != nil {
			return outcome, err
		}
		if err := s.persistBoundArtifactManifest(executionCtx, claim, run, policy, manifest); err != nil {
			return outcome, err
		}
		return outcome, nil
	}
	read := func(fn func(*CatalogReader) error) error {
		return s.catalogStore.ReadBoundCatalog(ctx, binding, "artifact", row.ID, func(reader *CatalogReader) error {
			if err := s.validateArtifactBindingTx(reader.tx, policy, &run, &claim, false); err != nil {
				return err
			}
			return fn(reader)
		})
	}
	for _, kind := range []string{"entry", "asset", "recognition", "manifest"} {
		after := uint(0)
		pageSize := CatalogBatchRows
		if kind == "recognition" {
			pageSize = 25
		}
		for {
			var items []models.CatalogArtifactBindingItem
			if err := s.catalogStore.readDB.WithContext(ctx).
				Where("binding_id = ? AND entity_kind = ? AND entity_id > ? AND status <> ? AND (status <> 'failed' OR (retryable = ? AND attempts < ? AND (next_attempt_at IS NULL OR next_attempt_at <= ?)))", row.ID, kind, after, "completed", true, catalogArtifactItemMaxAttempts, time.Now().UTC()).
				Order("entity_id").Limit(pageSize).Find(&items).Error; err != nil {
				return fail(err)
			}
			if len(items) == 0 {
				break
			}
			ids := make([]uint, 0, len(items))
			for _, item := range items {
				ids = append(ids, item.EntityID)
			}
			work := make([]catalogArtifactScopedWork, 0, len(items))
			producerEnabled := (kind != "entry" || policy.STRMEnabled) &&
				(kind != "asset" || (policy.STRMEnabled && policy.StorageType != models.StorageTypeLocal)) &&
				(kind != "recognition" || policy.Metadata)
			if !producerEnabled {
				// The exact incremental change can retire artifacts from a producer
				// that is now disabled. Completing the item as skipped authorizes
				// only that source identity's manifest cleanup; no writer runs.
				for _, item := range items {
					itemCopy := item
					work = append(work, catalogArtifactScopedWork{item: itemCopy, key: fmt.Sprintf("%s:%d", kind, item.EntityID), execute: func(context.Context) (string, error) { return "skipped", nil }})
				}
			}
			switch {
			case !producerEnabled:
			case kind == "entry":
				var rows []models.MediaLibraryEntry
				if err := read(func(r *CatalogReader) error {
					return r.Entries().Where("library_id = ? AND id IN ?", run.LibraryID, ids).Find(&rows).Error
				}); err != nil {
					return fail(err)
				}
				byID := make(map[uint]models.MediaLibraryEntry, len(rows))
				for _, item := range rows {
					byID[item.ID] = item
				}
				for _, item := range items {
					entry, exists := byID[item.EntityID]
					if !exists {
						work = append(work, catalogArtifactScopedWork{item: item, key: fmt.Sprintf("entry:%d", item.EntityID), execute: func(context.Context) (string, error) { return "skipped", nil }})
						continue
					}
					relative, _ := strmRelativePath(entry.RelativePath)
					entryCopy := entry
					work = append(work, catalogArtifactScopedWork{item: item, key: relative, execute: func(taskCtx context.Context) (string, error) {
						return executeWrite(taskCtx, func(m *artifactManifestIndex) (string, error) {
							return s.writeSTRM(taskCtx, root, run, entryCopy, m, verifier)
						})
					}})
				}
			case kind == "asset":
				var rows []models.MediaLibrarySourceAsset
				if err := read(func(r *CatalogReader) error {
					return r.SourceAssets().Where("library_id = ? AND active = ? AND id IN ?", run.LibraryID, true, ids).Find(&rows).Error
				}); err != nil {
					return fail(err)
				}
				byID := make(map[uint]models.MediaLibrarySourceAsset, len(rows))
				for _, item := range rows {
					byID[item.ID] = item
				}
				for _, item := range items {
					asset, exists := byID[item.EntityID]
					if !exists {
						work = append(work, catalogArtifactScopedWork{item: item, key: fmt.Sprintf("asset:%d", item.EntityID), execute: func(context.Context) (string, error) { return "skipped", nil }})
						continue
					}
					assetCopy := asset
					work = append(work, catalogArtifactScopedWork{item: item, key: asset.RelativePath, execute: func(taskCtx context.Context) (string, error) {
						return executeWrite(taskCtx, func(m *artifactManifestIndex) (string, error) {
							return s.writeSourceAsset(taskCtx, root, run, policy, assetCopy, m)
						})
					}})
				}
			case kind == "recognition":
				var rows []models.MediaLibraryRecognition
				if err := read(func(r *CatalogReader) error {
					return r.Recognitions().Where("library_id = ? AND status = ? AND id IN ?", run.LibraryID, mediaRecognitionStatusMatched, ids).Find(&rows).Error
				}); err != nil {
					return fail(err)
				}
				byID := make(map[uint]models.MediaLibraryRecognition, len(rows))
				for _, item := range rows {
					byID[item.ID] = item
				}
				var entryRows []models.MediaLibraryEntry
				if err := read(func(r *CatalogReader) error {
					return r.Entries().Where("library_id = ? AND recognition_id IN ?", run.LibraryID, ids).Order("recognition_id,relative_path").Find(&entryRows).Error
				}); err != nil {
					return fail(err)
				}
				entriesByRecognition := make(map[uint][]models.MediaLibraryEntry, len(rows))
				for _, entry := range entryRows {
					if entry.RecognitionID != nil {
						entriesByRecognition[*entry.RecognitionID] = append(entriesByRecognition[*entry.RecognitionID], entry)
					}
				}
				for _, item := range items {
					record, exists := byID[item.EntityID]
					if !exists {
						work = append(work, catalogArtifactScopedWork{item: item, key: fmt.Sprintf("recognition:%d", item.EntityID), execute: func(context.Context) (string, error) { return "skipped", nil }})
						continue
					}
					entries := entriesByRecognition[record.ID]
					if len(entries) == 0 {
						work = append(work, catalogArtifactScopedWork{item: item, key: fmt.Sprintf("recognition:%d", item.EntityID), execute: func(context.Context) (string, error) { return "skipped", nil }})
						continue
					}
					recordCopy, entriesCopy := record, append([]models.MediaLibraryEntry(nil), entries...)
					lane := fmt.Sprintf("recognition:%d", record.ID)
					if relative, err := nfoRelativePath(record.MediaType, entries); err == nil {
						lane = filepath.Dir(relative)
					}
					work = append(work, catalogArtifactScopedWork{item: item, key: lane, execute: func(taskCtx context.Context) (string, error) {
						return executeWrite(taskCtx, func(m *artifactManifestIndex) (string, error) {
							return s.writeNFO(taskCtx, root, run, policy.TargetKind, recordCopy, entriesCopy, m)
						})
					}})
				}
			case kind == "manifest":
				for _, item := range items {
					itemCopy := item
					work = append(work, catalogArtifactScopedWork{item: itemCopy, key: fmt.Sprintf("manifest:%d", item.EntityID), execute: func(context.Context) (string, error) { return "skipped", nil }})
				}
			}
			workItems := make([]models.CatalogArtifactBindingItem, 0, len(work))
			for index := range work {
				work[index].safePath = work[index].key
				workItem := work[index]
				workItems = append(workItems, workItem.item)
			}
			if err := s.startCatalogArtifactItems(ctx, policy, run, claim, workItems); err != nil {
				return fail(err)
			}
			workers := catalogArtifactLocalWorkers
			if kind == "asset" {
				workers = 1
			}
			buffer := make([]catalogArtifactScopedResult, 0, 16)
			flush := func() error {
				if len(buffer) == 0 {
					return nil
				}
				if err := s.finishCatalogArtifactItems(ctx, policy, run, claim, buffer); err != nil {
					return err
				}
				for _, result := range buffer {
					switch result.outcome {
					case "written":
						run.WrittenCount++
					case "updated":
						run.UpdatedCount++
					case "skipped":
						run.SkippedCount++
					}
					if result.err != nil {
						run.FailedCount++
					}
				}
				delta := len(buffer)
				buffer = buffer[:0]
				return heartbeat(delta)
			}
			executionErr := s.executeCatalogArtifactWork(ctx, work, workers, func(result catalogArtifactScopedResult) error {
				buffer = append(buffer, result)
				if len(buffer) >= cap(buffer) || catalogArtifactFatalError(result.err) {
					if err := flush(); err != nil {
						return err
					}
				}
				if catalogArtifactFatalError(result.err) {
					return result.err
				}
				return nil
			})
			if err := flush(); err != nil {
				return fail(err)
			}
			if executionErr != nil {
				return fail(executionErr)
			}
			after = items[len(items)-1].EntityID
		}
	}
	var failed int64
	if err := s.catalogStore.readDB.WithContext(ctx).Model(&models.CatalogArtifactBindingItem{}).Where("binding_id = ? AND status = ?", row.ID, "failed").Count(&failed).Error; err != nil {
		return fail(err)
	}
	run.FailedCount = int(failed)
	if run.FailedCount > 0 {
		return s.finishBoundArtifactFailure(ctx, run, policy)
	}
	// All physical writes and their manifests have succeeded before applied.
	if err := s.catalogArtifactWriteTx(ctx, func(tx *gorm.DB) error {
		if err := s.validateArtifactBindingTx(tx, policy, &run, &claim, true); err != nil {
			return err
		}
		now := time.Now().UTC()
		if err := tx.Model(&models.MediaArtifactRun{}).Where("id = ?", run.ID).Updates(map[string]any{"expected_count": run.ExpectedCount, "written_count": run.WrittenCount, "updated_count": run.UpdatedCount, "skipped_count": run.SkippedCount, "failed_count": 0, "processed_count": run.ExpectedCount, "succeeded_count": run.ExpectedCount, "updated_at": now}).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.MediaLibrary{}).Where("id = ?", run.LibraryID).Update("artifact_applied_generation", run.Generation).Error; err != nil {
			return err
		}
		return tx.Model(&models.CatalogArtifactBinding{}).Where("id = ?", row.ID).Updates(map[string]any{"state": "applying", "finalize_after_id": 0, "updated_at": now}).Error
	}); err != nil {
		return fail(err)
	}
	return s.finalizeBoundArtifacts(ctx, claim, run, policy, binding, fail)
}

func (s *MediaArtifactService) finishBoundArtifactFailure(ctx context.Context, run models.MediaArtifactRun, policy mediaArtifactPolicy) WorkerResult {
	now := time.Now().UTC()
	var retry struct {
		Count int64
		Next  *time.Time
	}
	if err := s.catalogStore.readDB.WithContext(ctx).Model(&models.CatalogArtifactBindingItem{}).
		Select("COUNT(*) AS count, MIN(next_attempt_at) AS next").
		Where("binding_id = ? AND status = ? AND retryable = ? AND attempts < ?", policy.CatalogBindingID, "failed", true, catalogArtifactItemMaxAttempts).
		Scan(&retry).Error; err != nil {
		return WorkerResult{ErrorCode: "artifact_state_unavailable", ErrorMessage: "媒体产物失败状态不可用"}
	}
	processed, succeeded := run.ExpectedCount, max(0, run.ExpectedCount-run.FailedCount)
	code := "artifact_write_failed"
	if retry.Count == 0 {
		code = "artifact_write_requires_action"
	}
	if err := s.catalogArtifactWriteTx(ctx, func(tx *gorm.DB) error {
		if err := tx.Model(&models.MediaArtifactRun{}).Where("id = ? AND catalog_binding_id = ?", run.ID, policy.CatalogBindingID).Updates(map[string]any{
			"status": models.MediaArtifactStatusFailed, "expected_count": run.ExpectedCount,
			"written_count": run.WrittenCount, "updated_count": run.UpdatedCount,
			"skipped_count": run.SkippedCount, "failed_count": run.FailedCount,
			"processed_count": processed, "succeeded_count": succeeded,
			"error_code": code, "cleanup_status": models.MediaArtifactCleanupSkipped,
			"cleanup_at": now, "finished_at": now, "updated_at": now,
		}).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.CatalogArtifactBinding{}).Where("id = ?", policy.CatalogBindingID).Updates(map[string]any{"state": "failed", "updated_at": now}).Error; err != nil {
			return err
		}
		return tx.Model(&models.MediaLibrary{}).Where("id = ? AND artifact_generation = ?", run.LibraryID, run.Generation).Updates(map[string]any{"artifact_status": models.MediaArtifactStatusFailed, "artifact_error": code, "artifact_updated_at": now}).Error
	}); err != nil {
		return WorkerResult{ErrorCode: "artifact_state_persist_failed", ErrorMessage: "媒体产物失败状态保存失败"}
	}
	if retry.Count == 0 {
		return WorkerResult{ErrorCode: code, ErrorMessage: "部分媒体产物无法自动处理，请查看失败项后重试"}
	}
	next := now.Add(time.Minute)
	if retry.Next != nil && retry.Next.After(now) {
		next = *retry.Next
	}
	return WorkerResult{RetryAt: &next, ErrorCode: code, ErrorMessage: "仅失败的媒体产物将在稍后重试"}
}

func (s *MediaArtifactService) persistBoundArtifactManifest(ctx context.Context, claim ClaimedJob, run models.MediaArtifactRun, policy mediaArtifactPolicy, manifest *artifactManifestIndex) error {
	if len(manifest.dirty) > CatalogBatchRows {
		return ErrCatalogBudget
	}
	return s.catalogArtifactWriteTx(ctx, func(tx *gorm.DB) error {
		if err := s.validateArtifactBindingTx(tx, policy, &run, &claim, true); err != nil {
			return err
		}
		for key := range manifest.dirty {
			artifact := manifest.rows[key]
			if artifact.Status != models.MediaArtifactStatusCompleted {
				return ErrCatalogInvalid
			}
			if err := tx.Save(&artifact).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *MediaArtifactService) finalizeBoundArtifacts(ctx context.Context, claim ClaimedJob, run models.MediaArtifactRun, policy mediaArtifactPolicy, binding CatalogSnapshotBinding, fail func(error) WorkerResult) WorkerResult {
	for {
		done := false
		err := s.catalogArtifactWriteTx(ctx, func(tx *gorm.DB) error {
			if err := s.validateArtifactBindingTx(tx, policy, &run, &claim, true); err != nil {
				return err
			}
			var row models.CatalogArtifactBinding
			if err := tx.First(&row, "id = ?", policy.CatalogBindingID).Error; err != nil {
				return err
			}
			if row.State != "applying" {
				return ErrCatalogFence
			}
			var ids []uint
			// A restarted run may contain rows written by an older binding of the
			// same durable run. They are owned by this execution and must always be
			// retired once the replacement binding is complete. CleanupEligible
			// only controls retirement of manifests owned by earlier runs.
			query := tx.Model(&models.MediaArtifact{}).Where("1 = 0")
			if !policy.ScanPartial {
				query = tx.Model(&models.MediaArtifact{}).Where("media_artifacts.library_id = ? AND media_artifacts.target_kind = ? AND media_artifacts.id > ? AND media_artifacts.catalog_binding_id <> ? AND media_artifacts.active = ? AND media_artifacts.run_id = ?", run.LibraryID, policy.TargetKind, row.FinalizeAfterID, row.ID, true, run.ID)
			}
			if row.ScopeMode == catalogArtifactScopeFull && policy.CleanupEligible {
				query = tx.Model(&models.MediaArtifact{}).Where("media_artifacts.library_id = ? AND media_artifacts.target_kind = ? AND media_artifacts.id > ? AND media_artifacts.catalog_binding_id <> ? AND media_artifacts.active = ?", run.LibraryID, policy.TargetKind, row.FinalizeAfterID, row.ID, true)
			} else if row.ScopeMode == catalogArtifactScopeIncremental {
				query = tx.Model(&models.MediaArtifact{}).Where("media_artifacts.library_id = ? AND media_artifacts.target_kind = ? AND media_artifacts.id > ? AND media_artifacts.catalog_binding_id <> ? AND media_artifacts.active = ?", run.LibraryID, policy.TargetKind, row.FinalizeAfterID, row.ID, true)
				scopePredicate := `EXISTS (
					SELECT 1 FROM catalog_artifact_binding_items work
					WHERE work.binding_id = ? AND work.status = 'completed' AND work.outcome <> 'noop' AND (
					 (work.entity_kind = 'entry' AND media_artifacts.source_identity = 'entry:' || work.entity_id) OR
					 (work.entity_kind = 'asset' AND media_artifacts.source_identity = 'asset:' || work.entity_id) OR
					 (work.entity_kind = 'recognition' AND (media_artifacts.source_identity = 'recognition:' || work.entity_id OR media_artifacts.source_identity LIKE 'recognition:' || work.entity_id || ':%')) OR
					 (work.entity_kind = 'manifest' AND media_artifacts.id = work.entity_id)
					)
				)`
				if !policy.ScanPartial {
					scopePredicate = "media_artifacts.run_id = ? OR " + scopePredicate
					query = query.Where(scopePredicate, run.ID, row.ID)
				} else {
					query = query.Where(scopePredicate, row.ID)
				}
			}
			if err := query.Order("media_artifacts.id").Limit(CatalogBatchRows).Pluck("media_artifacts.id", &ids).Error; err != nil {
				return err
			}
			now := time.Now().UTC()
			if len(ids) > 0 {
				if err := tx.Model(&models.MediaArtifact{}).Where("id IN ?", ids).Updates(map[string]any{"active": false, "updated_at": now}).Error; err != nil {
					return err
				}
				return tx.Model(&row).Updates(map[string]any{"finalize_after_id": ids[len(ids)-1], "updated_at": now}).Error
			}
			if err := tx.Model(&row).Updates(map[string]any{"state": "completed", "updated_at": now}).Error; err != nil {
				return err
			}
			if err := tx.Model(&models.MediaArtifactRun{}).Where("id = ?", run.ID).Updates(map[string]any{"status": models.MediaArtifactStatusCompleted, "finished_at": now, "updated_at": now, "error_code": ""}).Error; err != nil {
				return err
			}
			if err := tx.Model(&models.MediaLibrary{}).Where("id = ?", run.LibraryID).Updates(map[string]any{"artifact_status": models.MediaArtifactStatusCompleted, "artifact_error": "", "artifact_updated_at": now}).Error; err != nil {
				return err
			}
			if err := ReleaseCatalogBindingTx(tx, binding, "artifact", row.ID); err != nil {
				return err
			}
			done = true
			return nil
		})
		if err != nil {
			return fail(err)
		}
		if done {
			break
		}
	}
	return WorkerResult{}
}
