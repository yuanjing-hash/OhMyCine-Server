package services

import (
	"context"
	"encoding/json"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

type observationRecoveryContextKey struct{}

// Explicit envelopes retain private model fields omitted by public JSON tags.
type observationRecoveryEntry struct {
	Entry      models.MediaLibraryEntry
	ProviderID string
	WorkKey    string
}
type observationRecoveryRecognition struct {
	Recognition      models.MediaLibraryRecognition
	SourceKey        string
	InputFingerprint string
	MetadataJSON     string
}
type observationRecovery struct {
	scope        models.ProviderObservationScope
	epoch        uint64
	entries      map[uint]models.MediaLibraryEntry
	recognitions map[uint]models.MediaLibraryRecognition
}

// Recovery bindings are committed with the new catalog head and survive fact
// GC. They are NOT pathname aliases and never cross a root/config/source epoch.
func saveObservationCatalogBindingsTx(tx *gorm.DB, p ObservationPublication) error {
	return saveObservationRecoveryBindingsTx(tx, p, false, 0)
}

// Confirmed removals capture the latest effective metadata BEFORE changing the
// head. A positive observation's old envelope may predate a user's manual edit.
// These envelope writes share the removal/head/ack transaction and roll back
// with it; they are not another observation or publication.
func captureQuarkObservationRecoveryTx(ctx context.Context, tx *gorm.DB, library models.MediaLibrary, run *models.MediaLibraryScanRun) error {
	if run == nil || run.Kind != "provider_observation" {
		return nil
	}
	changeScope, ok := providerChangeScopeFromContext(ctx)
	if !ok || changeScope.VerifiedResult == nil || len(changeScope.VerifiedResult.DeletedProviderIDs) == 0 {
		return nil
	}
	var storage models.Storage
	if err := tx.First(&storage, library.StorageID).Error; err != nil {
		return err
	}
	if storage.Type != models.StorageTypeQuark {
		return nil
	}
	if err := requireCatalogTransaction(tx); err != nil {
		return err
	}
	if len(changeScope.VerifiedResult.DeletedProviderIDs) > CatalogBatchRows {
		return ErrCatalogBudget
	}
	recovery, _ := ctx.Value(observationRecoveryContextKey{}).(*observationRecovery)
	if recovery == nil || recovery.scope.Provider != models.StorageTypeQuark || recovery.scope.Kind != "library" {
		return ErrObservationInvalid
	}
	if _, err := validateObservationRecoveryTx(ctx, tx, library.ID, recovery.epoch); err != nil {
		return err
	}
	publication := ObservationPublication{Scope: recovery.scope}
	for _, id := range changeScope.VerifiedResult.DeletedProviderIDs {
		publication.Changes = append(publication.Changes, ObservationChange{Kind: "observation_missing", Fact: ObservationFact{ID: id}})
	}
	return saveObservationRecoveryBindingsTx(tx, publication, true, recovery.epoch)
}

func saveObservationRecoveryBindingsTx(tx *gorm.DB, p ObservationPublication, confirmedAbsence bool, expectedEpoch uint64) error {
	if p.Scope.Kind != "library" {
		return nil
	}
	ids := make([]string, 0, len(p.Changes))
	for _, c := range p.Changes {
		if !c.Fact.IsDir && (c.Kind == "observation_missing") == confirmedAbsence {
			ids = append(ids, c.Fact.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	if confirmedAbsence && len(ids) > CatalogBatchRows {
		return ErrCatalogBudget
	}
	reader, err := PinCatalogTx(tx, []uint{p.Scope.LibraryID})
	if err != nil {
		return err
	}
	head, ok := reader.Head(p.Scope.LibraryID)
	if !ok || head.Mode != "versioned" {
		if confirmedAbsence {
			return ErrObservationFence
		}
		return nil
	}
	if confirmedAbsence && head.SourceEpoch != expectedEpoch {
		return ErrObservationFence
	}
	var entries []models.MediaLibraryEntry
	query := reader.Entries().Where("provider_id IN ?", ids)
	if confirmedAbsence {
		query = query.Limit(CatalogBatchRows + 1)
	}
	if err = query.Find(&entries).Error; err != nil {
		return err
	}
	if confirmedAbsence && len(entries) > CatalogBatchRows {
		return ErrCatalogBudget
	}
	bytes := 0
	for _, entry := range entries {
		raw, err := json.Marshal(observationRecoveryEntry{Entry: entry, ProviderID: entry.ProviderID, WorkKey: entry.WorkKey})
		if err != nil {
			return err
		}
		recognition := ""
		if entry.RecognitionID != nil {
			var rows []models.MediaLibraryRecognition
			if err = reader.Recognitions().Where("id = ?", *entry.RecognitionID).Limit(1).Find(&rows).Error; err != nil {
				return err
			}
			if len(rows) == 1 {
				b, err := json.Marshal(observationRecoveryRecognition{Recognition: rows[0], SourceKey: rows[0].SourceKey, InputFingerprint: rows[0].InputFingerprint, MetadataJSON: rows[0].MetadataJSON})
				if err != nil {
					return err
				}
				recognition = string(b)
			}
		}
		if len(raw)+len(recognition) > CatalogBatchBytes {
			return ErrCatalogBudget
		}
		bytes += len(raw) + len(recognition)
		if confirmedAbsence && bytes > CatalogBatchBytes {
			return ErrCatalogBudget
		}
		current := observationCurrent(tx, p.Scope.ID).Model(&models.ProviderObservationItem{}).Where("resource_id = ?", entry.ProviderID)
		if confirmedAbsence {
			current = current.Where("downlisted_at IS NOT NULL")
		} else {
			current = current.Where("downlisted_at IS NULL")
		}
		updated := current.Updates(map[string]any{"catalog_source_epoch": head.SourceEpoch, "recovery_entry_json": string(raw), "recovery_recognition_json": recognition})
		if updated.Error != nil {
			return updated.Error
		}
		if confirmedAbsence && updated.RowsAffected != 1 {
			return ErrObservationFence
		}
	}
	return nil
}

func prepareObservationRecovery(ctx context.Context, db *gorm.DB, p ObservationPublication) (context.Context, error) {
	recovery := &observationRecovery{scope: p.Scope, entries: map[uint]models.MediaLibraryEntry{}, recognitions: map[uint]models.MediaLibraryRecognition{}}
	var head models.CatalogHead
	if err := db.WithContext(ctx).First(&head, "library_id = ?", p.Scope.LibraryID).Error; err != nil {
		return ctx, err
	}
	recovery.epoch = head.SourceEpoch
	for _, change := range p.Changes {
		if change.Kind != "restored" || change.Fact.IsDir {
			continue
		}
		var row models.ProviderObservationItem
		if err := observationCurrent(db.WithContext(ctx), p.Scope.ID).First(&row, "resource_id = ?", change.Fact.ID).Error; err != nil {
			return ctx, err
		}
		if row.RecoveryEntryJSON == "" || row.CatalogSourceEpoch != head.SourceEpoch {
			continue
		}
		var saved observationRecoveryEntry
		if json.Unmarshal([]byte(row.RecoveryEntryJSON), &saved) != nil {
			return ctx, ErrObservationInvalid
		}
		entry := saved.Entry
		entry.ProviderID, entry.WorkKey = saved.ProviderID, saved.WorkKey
		if entry.ID == 0 || entry.LibraryID != p.Scope.LibraryID || entry.ProviderID != change.Fact.ID {
			return ctx, ErrObservationInvalid
		}
		recovery.entries[entry.ID] = entry
		if row.RecoveryRecognitionJSON != "" {
			var saved observationRecoveryRecognition
			if json.Unmarshal([]byte(row.RecoveryRecognitionJSON), &saved) != nil {
				return ctx, ErrObservationInvalid
			}
			recognition := saved.Recognition
			recognition.SourceKey, recognition.InputFingerprint, recognition.MetadataJSON = saved.SourceKey, saved.InputFingerprint, saved.MetadataJSON
			if recognition.LibraryID != p.Scope.LibraryID || entry.RecognitionID == nil || recognition.ID != *entry.RecognitionID {
				return ctx, ErrObservationInvalid
			}
			recovery.recognitions[recognition.ID] = recognition
		}
	}
	return context.WithValue(ctx, observationRecoveryContextKey{}, recovery), nil
}

func validateObservationRecoveryTx(ctx context.Context, tx *gorm.DB, libraryID uint, epoch uint64) (*observationRecovery, error) {
	recovery, _ := ctx.Value(observationRecoveryContextKey{}).(*observationRecovery)
	if recovery == nil {
		return nil, nil
	}
	if recovery.scope.LibraryID != libraryID || recovery.epoch != epoch {
		return nil, ErrObservationFence
	}
	var scope models.ProviderObservationScope
	if err := tx.First(&scope, "id = ?", recovery.scope.ID).Error; err != nil {
		return nil, err
	}
	if !scope.Enabled || scope.Fence != recovery.scope.Fence || scope.RootID != recovery.scope.RootID || scope.AccountKey != recovery.scope.AccountKey {
		return nil, ErrObservationFence
	}
	return recovery, nil
}

func appendObservationRecoveryBaseline(ctx context.Context, db *gorm.DB, libraryID uint, baseline *catalogScanBaseline) error {
	recovery, _ := ctx.Value(observationRecoveryContextKey{}).(*observationRecovery)
	if recovery == nil || len(recovery.entries)+len(recovery.recognitions) == 0 {
		return nil
	}
	if len(recovery.entries) > CatalogBatchRows || len(recovery.recognitions) > CatalogBatchRows {
		return ErrCatalogBudget
	}
	// The caller supplies its deferred read pool. A partial scan baseline may
	// omit a still-live shared recognition, so absence in that slice is not
	// evidence of absence in the effective head. Check only these exact IDs.
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := validateObservationRecoveryTx(ctx, tx, libraryID, baseline.head.SourceEpoch); err != nil {
			return err
		}
		reader, err := PinCatalogTx(tx, []uint{libraryID})
		if err != nil {
			return err
		}
		head, ok := reader.Head(libraryID)
		if !ok || head.Mode != "versioned" || head.Revision != baseline.head.Revision || head.SourceEpoch != baseline.head.SourceEpoch || head.SourceFingerprint != baseline.head.SourceFingerprint || head.ConfigFingerprint != baseline.head.ConfigFingerprint {
			return ErrCatalogFence
		}
		entryIDs, recognitionIDs := []uint{}, []uint{}
		entrySeen, recognitionSeen := map[uint]bool{}, map[uint]bool{}
		for _, entry := range baseline.entries {
			entrySeen[entry.ID] = true
		}
		for _, record := range baseline.recognitions {
			recognitionSeen[record.ID] = true
		}
		for id := range recovery.entries {
			if !entrySeen[id] {
				entryIDs = append(entryIDs, id)
			}
		}
		for id := range recovery.recognitions {
			if !recognitionSeen[id] {
				recognitionIDs = append(recognitionIDs, id)
			}
		}
		if len(entryIDs) > 0 {
			var current []models.MediaLibraryEntry
			if err := reader.Entries().Where("library_id = ? AND id IN ?", libraryID, entryIDs).Limit(CatalogBatchRows).Find(&current).Error; err != nil {
				return err
			}
			for _, entry := range current {
				baseline.entries = append(baseline.entries, entry)
				entrySeen[entry.ID] = true
			}
			for _, id := range entryIDs {
				if !entrySeen[id] {
					baseline.entries = append(baseline.entries, recovery.entries[id])
					if baseline.restoredEntries == nil {
						baseline.restoredEntries = map[uint]bool{}
					}
					baseline.restoredEntries[id] = true
				}
			}
		}
		if len(recognitionIDs) > 0 {
			var current []models.MediaLibraryRecognition
			if err := reader.Recognitions().Where("library_id = ? AND id IN ?", libraryID, recognitionIDs).Limit(CatalogBatchRows).Find(&current).Error; err != nil {
				return err
			}
			for _, record := range current {
				baseline.recognitions = append(baseline.recognitions, record)
				recognitionSeen[record.ID] = true
			}
			for _, id := range recognitionIDs {
				if !recognitionSeen[id] {
					baseline.recognitions = append(baseline.recognitions, recovery.recognitions[id])
					if baseline.restoredRecognitions == nil {
						baseline.restoredRecognitions = map[uint]bool{}
					}
					baseline.restoredRecognitions[id] = true
				}
			}
		}
		return nil
	})
}

// Only explicit observed restoration can contribute retired identity evidence.
// The allocator still checks that the anchor belongs to this source epoch.
func appendObservationRecoveryIdentityEvidence(ctx context.Context, tx *gorm.DB, candidate models.CatalogSnapshot, kind string, ids []uint, evidence map[uint]catalogIdentitySource) error {
	recovery, err := validateObservationRecoveryTx(ctx, tx, candidate.LibraryID, candidate.SourceEpoch)
	if err != nil || recovery == nil {
		return err
	}
	for _, id := range ids {
		if _, ok := evidence[id]; ok {
			continue
		}
		if kind == "entry" {
			if entry, ok := recovery.entries[id]; ok {
				evidence[id] = catalogIdentitySource{ID: id, SourceKey: entry.RelativePath, ProviderID: entry.ProviderID}
			}
		}
		if kind == "recognition" {
			if r, ok := recovery.recognitions[id]; ok {
				evidence[id] = catalogIdentitySource{ID: id, SourceKey: r.SourceKey}
			}
		}
	}
	return nil
}
