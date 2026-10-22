package services

import (
	"errors"
	"path"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
)

// The domain proof, not the queue lease or physical quiescence, owns this
// narrow hold. Cancellation and unknown outcomes cannot release it. Other
// providers and libraries retain the ordinary physical-write contract.
func observationQuarkReorganizationHoldTx(tx *gorm.DB, scope models.ProviderObservationScope) error {
	if scope.Provider != models.ConnectionProviderQuark || scope.LibraryID == 0 {
		return nil
	}
	var row struct{ ID string }
	err := tx.Model(&models.MediaReorganizationTask{}).Select("id").Where("library_id = ?", scope.LibraryID).
		// Only Quark execution writes this versioned marker. A corrupt plan
		// cannot erase an existing hold; unknown state also fails closed.
		Where("CASE WHEN json_valid(state_json) THEN COALESCE(json_extract(state_json, '$.catalog.observation_hold_version'), 0) ELSE -1 END <> 0").
		Limit(1).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return ErrObservationDeferred
}

// ConfirmQuarkReorganizationObservationTx acknowledges facts ALREADY committed
// by finalizeCatalogReorganization's unique Catalog PublishTx. Its caller must
// clear the durable hold in this SAME transaction after this succeeds. This
// function neither publishes Catalog changes nor claims a complete listing.
func ConfirmQuarkReorganizationObservationTx(tx *gorm.DB, task models.MediaReorganizationTask, plan reorganizationPlan, state reorganizationState, facts []cloud.Item) error {
	if err := requireCatalogTransaction(tx); err != nil {
		return err
	}
	if task.ID == "" || plan.StorageType != models.StorageTypeQuark || plan.CatalogFence == nil || state.Catalog == nil || state.Catalog.ObservationHoldVersion != 1 || !state.Catalog.ExecutionStarted || state.Catalog.ExecutionProofVersion != 1 || task.LibraryID != plan.LibraryID {
		return ErrObservationFence
	}
	var durable models.MediaReorganizationTask
	if err := tx.First(&durable, "id = ?", task.ID).Error; err != nil {
		return err
	}
	var held reorganizationState
	var frozen reorganizationPlan
	if durable.LibraryID != task.LibraryID || durable.JobID != task.JobID || decodeStrictJSON(durable.StateJSON, &held) != nil || decodeStrictJSON(durable.PlanJSON, &frozen) != nil || !reflect.DeepEqual(frozen, plan) || held.Catalog == nil || held.Catalog.ObservationHoldVersion != 1 || !held.Catalog.ExecutionStarted || held.Catalog.ExecutionProofVersion != 1 || held.Catalog.BoundRevision != state.Catalog.BoundRevision {
		return ErrObservationFence
	}
	var owner models.CatalogPhysicalWrite
	if err := tx.First(&owner, "library_id = ? AND owner_kind = ? AND owner_id = ?", task.LibraryID, CatalogPhysicalReorganization, task.ID).Error; err != nil {
		return err
	}
	fence := plan.CatalogFence
	if owner.State != "entered" || owner.JobID != task.JobID || owner.SourceEpoch != fence.SourceEpoch || owner.SourceFingerprint != fence.SourceFingerprint || owner.ConfigFingerprint != fence.ConfigFingerprint {
		return ErrObservationFence
	}
	var head models.CatalogHead
	if err := tx.First(&head, "library_id = ?", task.LibraryID).Error; err != nil {
		return err
	}
	if head.Mode != "versioned" || head.SourceEpoch != fence.SourceEpoch || head.SourceFingerprint != fence.SourceFingerprint || head.ConfigFingerprint != fence.ConfigFingerprint || head.Revision <= state.Catalog.BoundRevision {
		return ErrObservationFence
	}
	var scopes []models.ProviderObservationScope
	if err := tx.Where("library_id = ? AND provider = ? AND kind = 'library' AND enabled = ?", task.LibraryID, models.ConnectionProviderQuark, true).Find(&scopes).Error; err != nil {
		return err
	}
	if len(scopes) == 0 {
		return nil
	}
	input, err := quarkObservationScopeTx(tx, task.LibraryID)
	if err != nil {
		return err
	}
	for _, scope := range scopes {
		if scope.Fence != input.Fence || scope.AccountKey != input.AccountKey || scope.ConnectionID != input.ConnectionID || scope.RootID != input.RootID || scope.RootPath != input.RootPath {
			return ErrObservationFence
		}
		if err := confirmReorganizationObservationFactsTx(tx, scope, plan, state, facts, time.Now().UTC()); err != nil {
			return err
		}
	}
	return nil
}

func confirmReorganizationObservationFactsTx(tx *gorm.DB, scope models.ProviderObservationScope, plan reorganizationPlan, state reorganizationState, facts []cloud.Item, now time.Time) error {
	if state.Catalog == nil || state.Catalog.Directories["."] != scope.RootID || len(plan.Items) == 0 || len(facts) > len(plan.Items)+len(state.Catalog.Directories) {
		return ErrObservationInvalid
	}
	byID := make(map[string]ObservationFact, len(facts))
	for _, item := range facts {
		if _, duplicate := byID[item.ID]; duplicate {
			return ErrObservationInvalid
		}
		byID[item.ID] = ObservationFact{ID: item.ID, ParentID: item.ParentID, Name: item.Name, IsDir: item.IsDir, Size: item.Size, ModifiedAt: item.ModifiedAt, ContentVersion: item.Revision, Digest: item.SHA1}
	}
	paths := make(map[string]string, len(facts))
	for relative, id := range state.Catalog.Directories {
		if relative == "." {
			continue
		}
		fact, ok := byID[id]
		if !ok || !fact.IsDir || path.Clean(relative) != relative || strings.HasPrefix(relative, "/") || relative == ".." || strings.HasPrefix(relative, "../") || !validObservationFact(fact, state.Catalog.Directories[path.Dir(relative)]) || fact.Name != path.Base(relative) {
			return ErrObservationFence
		}
		if _, duplicate := paths[id]; duplicate {
			return ErrObservationFence
		}
		paths[id] = relative
	}
	for _, item := range plan.Items {
		fact, ok := byID[item.ProviderItemID]
		if !ok || !state.Completed[item.ManagedItemID] || fact.IsDir || fact.Size != item.Size || path.Clean(item.NewRelativePath) != item.NewRelativePath || strings.HasPrefix(item.NewRelativePath, "/") || strings.HasPrefix(item.NewRelativePath, "../") || !validObservationFact(fact, state.Catalog.Directories[path.Dir(item.NewRelativePath)]) || fact.Name != path.Base(item.NewRelativePath) {
			return ErrObservationFence
		}
		if _, duplicate := paths[fact.ID]; duplicate {
			return ErrObservationFence
		}
		paths[fact.ID] = item.NewRelativePath
	}
	for id, fact := range byID {
		if id == scope.RootID && fact.IsDir {
			continue
		}
		if _, ok := paths[id]; !ok {
			return ErrObservationFence
		}
	}
	return confirmObservationOperationFactsTx(tx, scope, paths, byID, "reorganization_catalog_confirmed", now)
}

// Inputs have already been checked against the caller's own immutable plan,
// physical owner and fresh provider facts. This only advances observation
// baselines; it never publishes a second Catalog revision.
func confirmObservationOperationFactsTx(tx *gorm.DB, scope models.ProviderObservationScope, paths map[string]string, byID map[string]ObservationFact, reason string, now time.Time) error {
	ids := make([]string, 0, len(paths))
	for id := range paths {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return paths[ids[i]] < paths[ids[j]] })
	parents := map[string]bool{}
	for _, id := range ids {
		fact := byID[id]
		var row models.ProviderObservationItem
		err := observationCurrent(tx, scope.ID).First(&row, "resource_id = ?", id).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if row.ParentID != "" {
			parents[row.ParentID] = true
		}
		parents[fact.ParentID] = true
		row.ScopeID, row.BatchID, row.ResourceID = scope.ID, "", id
		row.ParentID, row.Name, row.IsDir, row.Size = fact.ParentID, fact.Name, fact.IsDir, fact.Size
		row.ModifiedAt, row.ContentVersion, row.Digest = fact.ModifiedAt, fact.ContentVersion, fact.Digest
		row.RelativePath, row.Fingerprint = path.Join("/", paths[id]), observationFingerprint(fact)
		row.SeenAt, row.UpdatedAt = now, now
		row.MissingSince, row.MissingLastAt, row.DownlistedAt, row.RecoveryUntil = nil, nil, nil, nil
		row.MissingObservations, row.MissingRound, row.MissingBatchID, row.AbsentParentRound = 0, 0, "", 0
		row.Change, row.Revision = "", 0
		if fact.IsDir {
			// An exact Stat proves this directory exists, not its children's
			// completeness. Newly created parents join the persistent queue.
			parents[id] = true
			if errors.Is(err, gorm.ErrRecordNotFound) {
				row.QueueRound, row.DueAt = scope.CoverageRound, now
			}
		}
		if err := observationUpsert(tx, &row); err != nil {
			return err
		}
		// Preserve observed facts for historical completeness, but the old
		// positive/absence deltas no longer own a Catalog publication.
		if err := tx.Model(&models.ProviderObservationItem{}).Where("scope_id = ? AND batch_id <> '' AND resource_id = ? AND change <> ''", scope.ID, id).Update("change", "").Error; err != nil {
			return err
		}
	}
	for parent := range parents {
		// In-flight pages, including empty ones, must restart from page zero;
		// neither an old page nor its missing-confirmation cursor survives.
		if err := tx.Model(&models.ProviderObservationBatch{}).Where("scope_id = ? AND directory_id = ? AND state IN ?", scope.ID, parent, []string{"reading", "confirming"}).Updates(map[string]any{"state": "incomplete", "incomplete_reason": reason, "updated_at": now}).Error; err != nil {
			return err
		}
		due := now.Add(5 * time.Second)
		if err := observationCurrent(tx, scope.ID).Model(&models.ProviderObservationItem{}).Where("resource_id = ? AND is_dir = ?", parent, true).Updates(map[string]any{"active_batch_id": "", "due_at": gorm.Expr("CASE WHEN due_at < ? THEN due_at ELSE ? END", due, due)}).Error; err != nil {
			return err
		}
		if err := observationHeatParentsTx(tx, scope.ID, parent, parent, now); err != nil {
			return err
		}
	}
	return nil
}
