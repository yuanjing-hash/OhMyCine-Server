package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const catalogStorageMutationLibraries = 4

func (s *MediaLibraryService) writeCatalogConfiguration(ctx context.Context, foreground bool, write func(*gorm.DB) error) error {
	if s.catalogStore == nil {
		return s.db.WithContext(ctx).Transaction(write)
	}
	if foreground {
		return s.catalogStore.Admission().WithForeground(ctx, func() error { return s.db.WithContext(ctx).Transaction(write) })
	}
	return s.catalogStore.writeCatalogBatch(ctx, write)
}

func (s *MediaLibraryService) refreshCatalogProfileLibraries(profileID uint) error {
	for after := uint(0); ; {
		var ids []uint
		readDB := s.db
		if s.catalogStore != nil {
			readDB = s.catalogStore.readDB
		}
		if err := readDB.Model(&models.MediaLibrary{}).Where("profile_id=? AND id>?", profileID, after).Order("id").Limit(CatalogBatchRows).Pluck("id", &ids).Error; err != nil {
			return err
		}
		for _, id := range ids {
			if err := s.writeCatalogConfiguration(context.Background(), false, func(tx *gorm.DB) error {
				var library models.MediaLibrary
				if err := tx.First(&library, id).Error; errors.Is(err, gorm.ErrRecordNotFound) {
					return nil
				} else if err != nil {
					return err
				}
				if library.ProfileID != profileID {
					return nil
				}
				retiring, err := mediaLibraryRetiringTx(tx, library.ID)
				if err != nil || retiring {
					return err
				}
				var profile models.MediaClassificationProfile
				if err := tx.First(&profile, profileID).Error; err != nil {
					return err
				}
				if library.ProfileRevision == profile.Revision {
					return nil
				}
				organization, err := storedProfileOrganizationConfig(profile)
				if err != nil {
					return err
				}
				var storage models.Storage
				if err := tx.First(&storage, library.StorageID).Error; err != nil {
					return err
				}
				replacement := library
				replacement.ReclassificationDue = true
				replacement.MovieDirectoryTemplate, replacement.MovieFilenameTemplate = organization.MovieDirectoryTemplate, organization.MovieFilenameTemplate
				replacement.TVDirectoryTemplate, replacement.TVFilenameTemplate = organization.TVDirectoryTemplate, organization.TVFilenameTemplate
				var mode string
				if err := tx.Model(&models.CatalogHead{}).Where("library_id=?", library.ID).Select("mode").Scan(&mode).Error; err != nil {
					return err
				}
				// Global Profile edits are already committed. A converting
				// library keeps its frozen head/manifest and fails its actual
				// Profile fence; it must not block propagation to later libraries.
				if mode != "converting" {
					if _, err := applyCatalogLibraryChangeTx(tx, library, &replacement, storage, storage, profile); err != nil {
						return err
					}
				}
				return tx.Model(&library).Updates(map[string]any{"reclassification_due": true, "movie_directory_template": replacement.MovieDirectoryTemplate, "movie_filename_template": replacement.MovieFilenameTemplate, "tv_directory_template": replacement.TVDirectoryTemplate, "tv_filename_template": replacement.TVFilenameTemplate}).Error
			}); err != nil {
				return err
			}
		}
		if len(ids) < CatalogBatchRows {
			return nil
		}
		after = ids[len(ids)-1]
	}
}

func catalogSourceFingerprint(library models.MediaLibrary, storage models.Storage) string {
	connectionID := uint(0)
	if storage.ConnectionID != nil {
		connectionID = *storage.ConnectionID
	}
	value := strings.Join([]string{"catalog-source-v1", uintID(library.ID), uintID(storage.ID), storage.Type, storage.RootPathNormalized, storage.RootPath, uintID(connectionID), strconv.FormatUint(storage.CatalogConnectionEpoch, 10), library.RelativeRoot, library.ProviderRootID}, "\x00")
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func catalogConfigFingerprint(library models.MediaLibrary, storage models.Storage, profile models.MediaClassificationProfile) string {
	library.DirtyGeneration = 0
	// ProfileRevision on the library is a scan-result cursor, not current rule
	// authority. Use actual Profile.Revision; ignore baseline/artifact counters.
	value := []string{"catalog-config-v1", mediaLibraryScanSourceFingerprint(library, storage, profile), strconv.FormatBool(library.Enabled), strconv.FormatBool(storage.Enabled), storage.Capabilities, library.MovieDirectoryTemplate, library.MovieFilenameTemplate, library.TVDirectoryTemplate, library.TVFilenameTemplate, library.STRMLocalRoot, strconv.FormatBool(library.STRMEnabled), strconv.FormatBool(library.SignedProxyEnabled), strconv.FormatBool(library.MetadataArtifactsEnabled), profile.RulesJSON, profile.BuiltinRecognitionPacksJSON, profile.RecognitionRulesJSON, profile.MovieDirectoryTemplate, profile.MovieFilenameTemplate, profile.TVDirectoryTemplate, profile.TVFilenameTemplate}
	encoded, _ := json.Marshal(value)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// Apply one library's configuration in the SAME outer writer as its config save.
// No media files, anchors, scan runs or old referenced facts are removed here.
func applyCatalogLibraryChangeTx(tx *gorm.DB, before models.MediaLibrary, after *models.MediaLibrary, oldStorage, nextStorage models.Storage, profile models.MediaClassificationProfile) (bool, error) {
	if err := requireCatalogTransaction(tx); err != nil {
		return false, err
	}
	if catalogSourceFingerprint(before, oldStorage) != catalogSourceFingerprint(*after, nextStorage) || catalogConfigFingerprint(before, oldStorage, profile) != catalogConfigFingerprint(*after, nextStorage, profile) {
		if err := requireMediaLibraryNotRetiringTx(tx, before.ID); err != nil {
			return false, err
		}
		if err := AssertCatalogPhysicalDrainedTx(tx, before.ID); err != nil {
			return false, err
		}
	}
	var head models.CatalogHead
	err := tx.First(&head, "library_id=?", before.ID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if head.Mode == "legacy" {
		return false, nil
	}
	if head.Mode != "versioned" {
		return true, ErrCatalogFence
	}
	floor, err := database.ReadCatalogFormat(tx.Statement.Context, tx)
	if err != nil {
		return true, err
	}
	if floor != CatalogFormat {
		return true, ErrCatalogInvalid
	}
	if head.Revision == math.MaxUint64 {
		return true, ErrCatalogBudget
	}
	sourceChanged := catalogSourceFingerprint(before, oldStorage) != catalogSourceFingerprint(*after, nextStorage)
	config := catalogConfigFingerprint(*after, nextStorage, profile)
	if !sourceChanged && head.ConfigFingerprint == config {
		return true, nil
	}
	now := time.Now().UTC()
	updates := map[string]any{"revision": head.Revision + 1, "config_fingerprint": config, "updated_at": now}
	if sourceChanged {
		if head.SourceEpoch == math.MaxUint64 || before.DirtyGeneration == math.MaxUint64 {
			return true, ErrCatalogBudget
		}
		source := catalogSourceFingerprint(*after, nextStorage)
		updates["source_epoch"], updates["source_fingerprint"] = head.SourceEpoch+1, source
		// Empty source publication is already complete: no private facts or
		// projection walk is required. It never claims a media scan succeeded.
		empty := models.CatalogSnapshot{ID: uuid.NewString(), LibraryID: head.LibraryID, Kind: "base", State: "published", ParentRevision: head.Revision, SourceEpoch: head.SourceEpoch + 1, SourceFingerprint: source, ConfigFingerprint: config, OwnerTokenHash: catalogTokenHash(uuid.NewString()), LeaseExpiresAt: now, PublishedRevision: head.Revision + 1, CreatedAt: now, UpdatedAt: now}
		if err := tx.Create(&empty).Error; err != nil {
			return true, err
		}
		if err := tx.Create(&models.CatalogCollectionPreparation{SnapshotID: empty.ID, State: "prepared"}).Error; err != nil {
			return true, err
		}
		if err := tx.Where("library_id=?", head.LibraryID).Delete(&models.CatalogHeadLayer{}).Error; err != nil {
			return true, err
		}
		if err := tx.Create(&models.CatalogHeadLayer{LibraryID: head.LibraryID, Rank: 0, SnapshotID: empty.ID}).Error; err != nil {
			return true, err
		}
		after.BaselineGeneration, after.DirtyGeneration = 0, before.DirtyGeneration+1
		after.LastScanAt, after.LastSuccessfulScanAt = nil, nil
		after.StructureStatus, after.StructureIssueCount, after.StructureErrorCode, after.StructureCheckedAt = models.MediaLibraryStructurePending, 0, "", nil
		if err := invalidateCatalogSourceDiagnosisTx(tx, head.LibraryID, now); err != nil {
			return true, err
		}
	}
	result := tx.Model(&models.CatalogHead{}).Where("library_id=? AND revision=?", head.LibraryID, head.Revision).Updates(updates)
	if result.Error != nil {
		return true, result.Error
	}
	if result.RowsAffected != 1 {
		return true, ErrCatalogFence
	}
	// At most eight globally admitted candidates. A source/config fence
	// revocation makes these owners obsolete even if their job lease is live.
	var candidates []models.CatalogSnapshot
	if err := tx.Select("id").Where("library_id=? AND state IN ('building','validating','ready')", head.LibraryID).Limit(CatalogMaxPreparations + 1).Find(&candidates).Error; err != nil {
		return true, err
	}
	if len(candidates) > CatalogMaxPreparations {
		return true, ErrCatalogBudget
	}
	for _, candidate := range candidates {
		if err := tx.Model(&candidate).Updates(map[string]any{"state": "abandoned", "updated_at": now}).Error; err != nil {
			return true, err
		}
		if err := releaseCatalogCompactionRefsTx(tx, candidate.ID); err != nil {
			return true, err
		}
	}
	return true, nil
}

func invalidateCatalogSourceDiagnosisTx(tx *gorm.DB, libraryID uint, now time.Time) error {
	// Keep the old issue rows and drafts for bounded cleanup. The current
	// diagnosis state/source revision prevents preview or execution of them.
	if err := tx.Model(&models.MediaLibraryStructureDiagnosis{}).Where("library_id=?", libraryID).Updates(map[string]any{"status": "failed", "issues_json": "[]", "issue_count": 0, "repairable_count": 0, "last_error_code": "source_changed", "updated_at": now}).Error; err != nil {
		return err
	}
	auto := models.MediaLibraryStructureAutoState{LibraryID: libraryID, SourceRevision: 1, Status: "pending", UpdatedAt: now}
	return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "library_id"}}, DoUpdates: clause.Assignments(map[string]any{"source_revision": gorm.Expr("source_revision+1"), "status": "pending", "updated_at": now})}).Create(&auto).Error
}

func applyCatalogStorageChangeTx(tx *gorm.DB, expected, next models.Storage, changes ...*MediaChangeService) error {
	var previous models.Storage
	if err := tx.First(&previous, next.ID).Error; err != nil {
		return err
	}
	if catalogSourceFingerprint(models.MediaLibrary{}, previous) != catalogSourceFingerprint(models.MediaLibrary{}, expected) || previous.CatalogConnectionRevision != expected.CatalogConnectionRevision || previous.Enabled != expected.Enabled || previous.Name != expected.Name || previous.Capabilities != expected.Capabilities {
		return appError(CodeConflict, "存储配置已变化，请刷新后重试", ErrCatalogFence)
	}
	// Cosmetic storage updates do not touch catalogs or impose a library cap.
	if catalogSourceFingerprint(models.MediaLibrary{}, previous) == catalogSourceFingerprint(models.MediaLibrary{}, next) && previous.CatalogConnectionRevision == next.CatalogConnectionRevision && previous.Enabled == next.Enabled && previous.Capabilities == next.Capabilities {
		return nil
	}
	if err := requireCatalogScopeNotRetiringTx(tx, "storage_id=?", next.ID); err != nil {
		return err
	}
	if err := assertCatalogPhysicalScopeDrainedTx(tx, "storage_id=?", next.ID); err != nil {
		return err
	}
	var libraries []models.MediaLibrary
	if err := tx.Model(&models.MediaLibrary{}).Joins("JOIN catalog_heads h ON h.library_id=media_libraries.id AND h.mode IN ('versioned','converting')").Where("media_libraries.storage_id=?", next.ID).Order("media_libraries.id").Limit(catalogStorageMutationLibraries + 1).Find(&libraries).Error; err != nil {
		return err
	}
	if len(libraries) > catalogStorageMutationLibraries {
		return appError(CodeConflict, "此存储关联的媒体库较多，请先拆分存储来源后再修改根目录或启用状态", ErrCatalogBudget)
	}
	for _, library := range libraries {
		var profile models.MediaClassificationProfile
		if err := tx.First(&profile, library.ProfileID).Error; err != nil {
			return err
		}
		replacement := library
		if _, err := applyCatalogLibraryChangeTx(tx, library, &replacement, previous, next, profile); err != nil {
			return err
		}
		if err := tx.Model(&models.MediaLibrary{}).Where("id=?", library.ID).Updates(map[string]any{"baseline_generation": replacement.BaselineGeneration, "dirty_generation": replacement.DirtyGeneration, "last_scan_at": replacement.LastScanAt, "last_successful_scan_at": replacement.LastSuccessfulScanAt, "structure_status": replacement.StructureStatus, "structure_issue_count": replacement.StructureIssueCount, "structure_error_code": replacement.StructureErrorCode, "structure_checked_at": replacement.StructureCheckedAt}).Error; err != nil {
			return err
		}
		if catalogSourceFingerprint(library, previous) != catalogSourceFingerprint(replacement, next) {
			if len(changes) == 0 || changes[0] == nil {
				return ErrCatalogInvalid
			}
			if _, err := changes[0].RecordTx(tx, library.ID, replacement.DirtyGeneration, models.MediaLibraryChangeRemoval, true); err != nil {
				return err
			}
		}
	}
	return nil
}

func saveStorageProbeTx(tx *gorm.DB, probed models.Storage) error {
	var current models.Storage
	if err := tx.First(&current, probed.ID).Error; err != nil {
		return err
	}
	if catalogSourceFingerprint(models.MediaLibrary{}, current) != catalogSourceFingerprint(models.MediaLibrary{}, probed) || current.CatalogConnectionRevision != probed.CatalogConnectionRevision || current.Enabled != probed.Enabled {
		return appError(CodeConflict, "存储来源已变化，请重新检测", ErrCatalogFence)
	}
	return tx.Model(&current).Updates(map[string]any{"last_probe_exists": probed.LastProbeExists, "last_probe_readable": probed.LastProbeReadable, "last_probe_available": probed.LastProbeAvailable, "last_probe_free_bytes": probed.LastProbeFreeBytes, "last_probe_total_bytes": probed.LastProbeTotalBytes, "last_probe_error_code": probed.LastProbeErrorCode, "last_probe_checked_at": probed.LastProbeCheckedAt}).Error
}
