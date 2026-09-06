package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/updater"
	"gorm.io/gorm"
)

const catalogConversionJobType = "catalog_conversion"

// Deliberately package-private with no production caller or scheduler handler.
// Rollout requires the complete consumer inventory and performance acceptance;
// the real startup capability does not replace the drain/authorization guard.
type catalogConversionInput struct {
	LibraryID     uint
	Job           ClaimedJob
	Compatibility *updater.CatalogCompatibility
	Guard         func(*gorm.DB, uint) error
}

type catalogConversionPayload struct {
	Version   int  `json:"version"`
	LibraryID uint `json:"library_id"`
}

type catalogConversionReceipt struct {
	Kind              string `json:"kind"`
	Version           int    `json:"version"`
	LibraryID         uint   `json:"library_id"`
	SourceEpoch       uint64 `json:"source_epoch"`
	SourceFingerprint string `json:"source_fingerprint"`
	PublishedRevision uint64 `json:"published_revision"`
}

func catalogConversionFence(library models.MediaLibrary, storage models.Storage, profile models.MediaClassificationProfile) string {
	value := strings.Join([]string{catalogSourceFingerprint(library, storage), catalogConfigFingerprint(library, storage, profile), strconv.FormatUint(library.DirtyGeneration, 10), strconv.FormatUint(library.BaselineGeneration, 10), strconv.FormatUint(library.ContentRevision, 10), strconv.FormatUint(library.ProfileRevision, 10)}, "\x00")
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func catalogConversionContextTx(tx *gorm.DB, libraryID uint) (models.MediaLibrary, models.Storage, models.MediaClassificationProfile, error) {
	var library models.MediaLibrary
	var storage models.Storage
	var profile models.MediaClassificationProfile
	if err := tx.First(&library, libraryID).Error; err != nil {
		return library, storage, profile, err
	}
	if err := tx.First(&storage, library.StorageID).Error; err != nil {
		return library, storage, profile, err
	}
	err := tx.First(&profile, library.ProfileID).Error
	return library, storage, profile, err
}

func catalogConversionJobTx(tx *gorm.DB, input catalogConversionInput) error {
	var job models.Job
	if input.LibraryID == 0 || input.Job.Job.ID == "" || input.Job.LeaseToken == "" {
		return ErrCatalogInvalid
	}
	if err := tx.First(&job, "id=?", input.Job.Job.ID).Error; err != nil {
		return err
	}
	var payload catalogConversionPayload
	if job.JobType != catalogConversionJobType || job.ResourceKey != mediaArtifactResourceKey(input.LibraryID) || decodeStrictJSON(job.PayloadJSON, &payload) != nil || payload.Version != 1 || payload.LibraryID != input.LibraryID {
		return ErrCatalogFence
	}
	return catalogCheckJob(tx, models.CatalogSnapshot{JobID: &job.ID, JobLeaseHash: leaseHash(input.Job.LeaseToken)}, time.Now().UTC())
}

func (s *CatalogSnapshotStore) checkConversionCompatibility(ctx context.Context, compatibility *updater.CatalogCompatibility) error {
	var databases []struct{ Name, File string }
	if err := s.readDB.WithContext(ctx).Raw("PRAGMA database_list").Scan(&databases).Error; err != nil {
		return err
	}
	for _, db := range databases {
		if db.Name == "main" && db.File != "" {
			if err := compatibility.ValidateCatalogDatabase(db.File); err != nil {
				return err
			}
			return compatibility.EnsureCatalogFormat()
		}
	}
	return ErrCatalogInvalid
}

// startCatalogConversion installs the library writer fence and an owned private
// candidate. Takeover requires this SAME durable job with a revoked old lease,
// not any job that happens to possess a valid lease token.
func (s *CatalogSnapshotStore) startCatalogConversion(ctx context.Context, input catalogConversionInput) (models.CatalogSnapshot, string, error) {
	var candidate models.CatalogSnapshot
	if input.Guard == nil || input.Compatibility == nil {
		return candidate, "", ErrCatalogInvalid
	}
	if err := s.readDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := catalogConversionJobTx(tx, input); err != nil {
			return err
		}
		if err := assertCatalogConversionDrainedTx(tx, input.LibraryID); err != nil {
			return err
		}
		return input.Guard(tx, input.LibraryID)
	}); err != nil {
		return candidate, "", err
	}
	if err := s.checkConversionCompatibility(ctx, input.Compatibility); err != nil {
		return candidate, "", err
	}
	token := uuid.NewString() + uuid.NewString()
	err := s.writeCatalogGrowth(ctx, CatalogBatchBytes, func(tx *gorm.DB) error {
		if err := catalogConversionJobTx(tx, input); err != nil {
			return err
		}
		if err := assertCatalogConversionDrainedTx(tx, input.LibraryID); err != nil {
			return err
		}
		if err := input.Guard(tx, input.LibraryID); err != nil {
			return err
		}
		library, storage, profile, err := catalogConversionContextTx(tx, input.LibraryID)
		if err != nil {
			return err
		}
		var head models.CatalogHead
		err = tx.First(&head, "library_id=?", input.LibraryID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			head = models.CatalogHead{LibraryID: input.LibraryID, Mode: "converting", SourceEpoch: 1, SourceFingerprint: catalogSourceFingerprint(library, storage), ConfigFingerprint: catalogConfigFingerprint(library, storage, profile), UpdatedAt: time.Now().UTC()}
			if err := tx.Create(&head).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if head.Mode == "legacy" {
			var existing int64
			if err := tx.Model(&models.CatalogSnapshot{}).Where("library_id=? AND state NOT IN ('abandoned','gc')", input.LibraryID).Count(&existing).Error; err != nil {
				return err
			}
			if existing != 0 {
				return ErrCatalogFence
			}
			head.Mode, head.SourceFingerprint, head.ConfigFingerprint = "converting", catalogSourceFingerprint(library, storage), catalogConfigFingerprint(library, storage, profile)
			if err := tx.Model(&head).Updates(map[string]any{"mode": head.Mode, "source_fingerprint": head.SourceFingerprint, "config_fingerprint": head.ConfigFingerprint, "updated_at": time.Now().UTC()}).Error; err != nil {
				return err
			}
		} else if head.Mode != "converting" {
			return ErrCatalogFence
		}
		if head.SourceFingerprint != catalogSourceFingerprint(library, storage) || head.ConfigFingerprint != catalogConfigFingerprint(library, storage, profile) {
			return ErrCatalogFence
		}
		var active []models.CatalogSnapshot
		if err := tx.Where("library_id=? AND kind='base' AND state IN ('building','validating','ready')", input.LibraryID).Limit(2).Find(&active).Error; err != nil {
			return err
		}
		if len(active) > 1 {
			return ErrCatalogInvalid
		}
		now := time.Now().UTC()
		if len(active) == 1 {
			candidate = active[0]
			if candidate.JobID == nil || *candidate.JobID != input.Job.Job.ID || candidate.JobLeaseHash == leaseHash(input.Job.LeaseToken) {
				return ErrCatalogFence
			}
			if err := catalogCheckJob(tx, candidate, now); err == nil {
				return ErrCatalogFence
			} else if !errors.Is(err, ErrCatalogFence) {
				return err
			}
			if candidate.SourceEpoch != head.SourceEpoch || candidate.SourceFingerprint != head.SourceFingerprint || candidate.ConfigFingerprint != head.ConfigFingerprint {
				return ErrCatalogFence
			}
			candidate.OwnerTokenHash, candidate.JobLeaseHash, candidate.LeaseExpiresAt = catalogTokenHash(token), leaseHash(input.Job.LeaseToken), now.Add(time.Minute)
			return tx.Model(&candidate).Updates(map[string]any{"owner_token_hash": candidate.OwnerTokenHash, "job_lease_hash": candidate.JobLeaseHash, "lease_expires_at": candidate.LeaseExpiresAt, "updated_at": now}).Error
		}
		if err := catalogPreparationBudgetTx(tx, CatalogCandidateInput{LibraryID: input.LibraryID, Kind: "base"}); err != nil {
			return err
		}
		jobID := input.Job.Job.ID
		candidate = models.CatalogSnapshot{ID: uuid.NewString(), LibraryID: input.LibraryID, Kind: "base", State: "building", ParentRevision: head.Revision, SourceEpoch: head.SourceEpoch, SourceFingerprint: head.SourceFingerprint, ConfigFingerprint: head.ConfigFingerprint, OwnerTokenHash: catalogTokenHash(token), JobID: &jobID, JobLeaseHash: leaseHash(input.Job.LeaseToken), LeaseExpiresAt: now.Add(time.Minute), CreatedAt: now, UpdatedAt: now}
		if err := tx.Create(&candidate).Error; err != nil {
			return err
		}
		return tx.Create(&models.CatalogConversionManifest{SnapshotID: candidate.ID, State: "capturing", FenceDigest: catalogConversionFence(library, storage, profile), UpdatedAt: now}).Error
	})
	return candidate, token, err
}

func validateCatalogConversionTx(tx *gorm.DB, candidate models.CatalogSnapshot, token string, input catalogConversionInput) (models.CatalogConversionManifest, error) {
	var manifest models.CatalogConversionManifest
	if _, err := catalogOwnedCandidate(tx, candidate.ID, token, candidate.State); err != nil {
		return manifest, err
	}
	if err := catalogConversionJobTx(tx, input); err != nil {
		return manifest, err
	}
	if err := tx.First(&manifest, "snapshot_id=?", candidate.ID).Error; err != nil {
		return manifest, err
	}
	library, storage, profile, err := catalogConversionContextTx(tx, candidate.LibraryID)
	if err != nil {
		return manifest, err
	}
	if catalogConversionFence(library, storage, profile) != manifest.FenceDigest {
		return manifest, ErrCatalogFence
	}
	return manifest, nil
}

func (s *CatalogSnapshotStore) convertLegacyCatalog(ctx context.Context, input catalogConversionInput) (models.CatalogHead, error) {
	var head models.CatalogHead
	if completed, err := s.completedCatalogConversion(ctx, input); err != nil {
		return head, err
	} else if completed != nil {
		return *completed, nil
	}
	candidate, token, err := s.startCatalogConversion(ctx, input)
	if err != nil {
		return head, err
	}
	// Failed/interrupted preparation remains private with its exact checkpoint.
	// A successor claimed lease may resume; no timeout turns it into publication.
	if err := s.captureCatalogConversionInput(ctx, candidate, token, input); err != nil {
		return head, err
	}
	if candidate.State == "building" {
		if err := s.copyCatalogConversionFacts(ctx, candidate, token, input); err != nil {
			return head, err
		}
	}
	if err := s.verifyCatalogConversion(ctx, candidate, token, input); err != nil {
		return head, err
	}
	if candidate.State != "ready" {
		if err := s.Seal(ctx, candidate.ID, token); err != nil {
			return head, err
		}
		candidate.State = "ready"
	}
	if err := s.checkConversionCompatibility(ctx, input.Compatibility); err != nil {
		return head, err
	}
	err = s.writeCatalogBatch(ctx, func(tx *gorm.DB) error {
		_, err := s.PublishTx(tx, candidate.ID, token, candidate.ParentRevision, func(tx *gorm.DB) error {
			manifest, err := validateCatalogConversionTx(tx, candidate, token, input)
			if err != nil {
				return err
			}
			if manifest.State != "verified" || manifest.InputDigest == "" {
				return ErrCatalogInvalid
			}
			var actual models.CatalogSnapshot
			var prepared models.CatalogCollectionPreparation
			if err := tx.First(&actual, "id=?", candidate.ID).Error; err != nil {
				return err
			}
			if err := tx.First(&prepared, "snapshot_id=?", candidate.ID).Error; err != nil {
				return err
			}
			if actual.RowCount != manifest.InputRows+prepared.RowCount {
				return ErrCatalogInvalid
			}
			if err := assertCatalogConversionDrainedTx(tx, input.LibraryID); err != nil {
				return err
			}
			return input.Guard(tx, input.LibraryID)
		})
		if err != nil {
			return err
		}
		if err := tx.First(&head, "library_id=?", candidate.LibraryID).Error; err != nil {
			return err
		}
		// This job owns its checkpoint. Publication and the receipt commit
		// together, so a crash before queue ACK cannot rerun the conversion.
		payload, err := json.Marshal(catalogConversionReceipt{Kind: "catalog_conversion_published", Version: 1, LibraryID: head.LibraryID, SourceEpoch: head.SourceEpoch, SourceFingerprint: head.SourceFingerprint, PublishedRevision: head.Revision})
		if err != nil {
			return err
		}
		return tx.Model(&models.Job{}).Where("id=?", input.Job.Job.ID).Update("checkpoint_json", string(payload)).Error
	})
	return head, err
}

func (s *CatalogSnapshotStore) completedCatalogConversion(ctx context.Context, input catalogConversionInput) (*models.CatalogHead, error) {
	if input.Guard == nil || input.Compatibility == nil {
		return nil, ErrCatalogInvalid
	}
	var completed *models.CatalogHead
	err := s.readDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := catalogConversionJobTx(tx, input); err != nil {
			return err
		}
		if err := input.Guard(tx, input.LibraryID); err != nil {
			return err
		}
		var job models.Job
		if err := tx.First(&job, "id=?", input.Job.Job.ID).Error; err != nil {
			return err
		}
		if job.CheckpointJSON == "" || job.CheckpointJSON == "{}" {
			return nil
		}
		var receipt catalogConversionReceipt
		if decodeStrictJSON(job.CheckpointJSON, &receipt) != nil || receipt.Kind != "catalog_conversion_published" || receipt.Version != 1 || receipt.LibraryID != input.LibraryID || receipt.SourceEpoch == 0 || receipt.PublishedRevision == 0 || receipt.SourceFingerprint == "" {
			return ErrCatalogInvalid
		}
		var head models.CatalogHead
		if err := tx.First(&head, "library_id=?", input.LibraryID).Error; err != nil {
			return err
		}
		if head.Mode != "versioned" || head.SourceEpoch != receipt.SourceEpoch || head.SourceFingerprint != receipt.SourceFingerprint || head.Revision < receipt.PublishedRevision {
			return ErrCatalogFence
		}
		completed = &head
		return nil
	})
	if err != nil || completed == nil {
		return nil, err
	}
	if err := s.checkConversionCompatibility(ctx, input.Compatibility); err != nil {
		return nil, err
	}
	return completed, nil
}
