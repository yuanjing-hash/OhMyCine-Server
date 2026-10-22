package services

import (
	"errors"
	"path"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
)

func observationQuarkStructureHoldTx(tx *gorm.DB, scope models.ProviderObservationScope) error {
	if scope.Provider != models.ConnectionProviderQuark || scope.LibraryID == 0 {
		return nil
	}
	var row struct{ ID string }
	err := tx.Model(&models.MediaLibraryStructureRepair{}).Select("id").Where("library_id = ?", scope.LibraryID).
		Where("CASE WHEN json_valid(state_json) THEN COALESCE(json_extract(state_json, '$.observation_hold_version'), 0) ELSE -1 END <> 0").Limit(1).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return ErrObservationDeferred
}

// Called after the unique structure Catalog PublishTx, before releasing its
// logical hold in that same transaction. A repair is never a transfer/reorg task.
func confirmQuarkStructureObservationTx(tx *gorm.DB, repair models.MediaLibraryStructureRepair, plan StructurePlan, state structureCatalogRepairState, evidence quarkStructureEvidence) error {
	if err := requireCatalogTransaction(tx); err != nil {
		return err
	}
	if state.ObservationHoldVersion != 1 || state.QuarkMutation != nil || state.Fence.LibraryID != repair.LibraryID || len(plan.RecycleItems) != 0 {
		return ErrObservationFence
	}
	var durable models.MediaLibraryStructureRepair
	if err := tx.First(&durable, "id = ?", repair.ID).Error; err != nil {
		return err
	}
	var held structureCatalogRepairState
	if durable.LibraryID != repair.LibraryID || durable.PlanJSON != repair.PlanJSON || decodeStrictJSON(durable.StateJSON, &held) != nil || held.ObservationHoldVersion != 1 || held.QuarkMutation != nil || held.Fence != state.Fence || held.BoundRevision != state.BoundRevision {
		return ErrObservationFence
	}
	var owner models.CatalogPhysicalWrite
	if err := tx.First(&owner, "library_id = ? AND owner_kind = ? AND owner_id = ?", repair.LibraryID, CatalogPhysicalRepair, repair.ID).Error; err != nil {
		return err
	}
	if owner.SourceEpoch != state.Fence.SourceEpoch || owner.SourceFingerprint != state.Fence.SourceFingerprint || owner.ConfigFingerprint != state.Fence.ConfigFingerprint {
		return ErrObservationFence
	}
	if state.cancelPermit != nil {
		if err := validateCancelledStructurePermitTx(tx, *state.cancelPermit, repair); err != nil {
			return err
		}
	} else if owner.State != "entered" {
		return ErrObservationFence
	}
	var head models.CatalogHead
	if err := tx.First(&head, "library_id = ?", repair.LibraryID).Error; err != nil {
		return err
	}
	if head.Mode != "versioned" || head.SourceEpoch != state.Fence.SourceEpoch || head.SourceFingerprint != state.Fence.SourceFingerprint || head.ConfigFingerprint != state.Fence.ConfigFingerprint || head.Revision < state.BoundRevision || len(plan.Items) > 0 && head.Revision == state.BoundRevision {
		return ErrObservationFence
	}
	// The successful subset must come from real ordinal checkpoints of the
	// frozen plan, not from a fresh listing or invented management identities.
	var frozen StructurePlan
	if decodeStrictJSON(repair.PlanJSON, &frozen) != nil {
		return ErrObservationFence
	}
	authorized := map[string]StructurePlanItem{}
	for _, item := range frozen.Items {
		if _, exists := authorized[item.ProviderID]; exists {
			return ErrObservationFence
		}
		authorized[item.ProviderID] = item
	}
	for _, item := range plan.Items {
		expected, ok := authorized[item.ProviderID]
		// Planner-only display fields are intentionally absent from PlanJSON.
		item.Title, item.RecognitionID = "", 0
		if !ok || expected != item {
			return ErrObservationFence
		}
		var count int64
		if err := tx.Model(&models.MediaLibraryStructureRepairItem{}).Where("repair_id = ? AND action = 'move' AND source_relative = ? AND target_relative = ? AND status = ?", repair.ID, item.SourceRelative, item.TargetRelative, structureRepairItemSucceeded).Count(&count).Error; err != nil {
			return err
		}
		if count != 1 {
			return ErrObservationFence
		}
	}
	var scopes []models.ProviderObservationScope
	if err := tx.Where("library_id = ? AND provider = ? AND kind = 'library' AND enabled = ?", repair.LibraryID, models.ConnectionProviderQuark, true).Find(&scopes).Error; err != nil {
		return err
	}
	if len(scopes) == 0 {
		return nil
	}
	input, err := quarkObservationScopeTx(tx, repair.LibraryID)
	if err != nil {
		return err
	}
	for _, scope := range scopes {
		if scope.Fence != input.Fence || scope.AccountKey != input.AccountKey || scope.ConnectionID != input.ConnectionID || scope.RootID != input.RootID || scope.RootPath != input.RootPath {
			return ErrObservationFence
		}
		paths, byID, err := quarkStructureObservationFacts(scope.RootID, plan, evidence)
		if err != nil {
			return err
		}
		if err := confirmObservationOperationFactsTx(tx, scope, paths, byID, "structure_catalog_confirmed", time.Now().UTC()); err != nil {
			return err
		}
	}
	return nil
}

type quarkStructureEvidence struct {
	Directories map[string]string
	Facts       []cloud.Item
}

func quarkStructureObservationFacts(root string, plan StructurePlan, evidence quarkStructureEvidence) (map[string]string, map[string]ObservationFact, error) {
	if evidence.Directories["."] != root || len(evidence.Facts) > len(plan.Items)+len(evidence.Directories) {
		return nil, nil, ErrObservationFence
	}
	byID := map[string]ObservationFact{}
	paths := map[string]string{}
	for _, i := range evidence.Facts {
		if _, dup := byID[i.ID]; dup {
			return nil, nil, ErrObservationFence
		}
		byID[i.ID] = ObservationFact{ID: i.ID, ParentID: i.ParentID, Name: i.Name, IsDir: i.IsDir, Size: i.Size, ModifiedAt: i.ModifiedAt, ContentVersion: i.Revision, Digest: i.SHA1}
	}
	for relative, id := range evidence.Directories {
		if relative == "." || relative == "" {
			continue
		}
		f, ok := byID[id]
		if !ok || !f.IsDir || safeStructurePath(relative) != relative || f.Name != path.Base(relative) || !validObservationFact(f, evidence.Directories[path.Dir(relative)]) {
			return nil, nil, ErrObservationFence
		}
		if _, dup := paths[id]; dup {
			return nil, nil, ErrObservationFence
		}
		paths[id] = relative
	}
	for _, item := range plan.Items {
		f, ok := byID[item.ProviderID]
		if !ok || f.IsDir || f.Size != item.Size || f.Name != path.Base(item.TargetRelative) || !validObservationFact(f, evidence.Directories[path.Dir(item.TargetRelative)]) {
			return nil, nil, ErrObservationFence
		}
		if _, dup := paths[f.ID]; dup {
			return nil, nil, ErrObservationFence
		}
		paths[f.ID] = item.TargetRelative
	}
	for id := range byID {
		if _, ok := paths[id]; !ok {
			return nil, nil, ErrObservationFence
		}
	}
	return paths, byID, nil
}
