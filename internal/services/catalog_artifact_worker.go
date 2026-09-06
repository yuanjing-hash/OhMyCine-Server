package services

import (
	"context"
	"errors"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	storagefs "github.com/yuanjing-hash/OhMyCine-Server/internal/storage"
	"gorm.io/gorm"
)

// The source snapshot is immutable; each read page and manifest writer is
// bounded. No catalog transaction spans rendering, downloads or physical I/O.
func (s *MediaArtifactService) generateBoundArtifacts(ctx context.Context, runtime JobRuntime, claim ClaimedJob, permit CatalogPhysicalWritePermit, run models.MediaArtifactRun, policy mediaArtifactPolicy) WorkerResult {
	fail := func(err error) WorkerResult {
		code := "artifact_snapshot_failed"
		if errors.Is(err, ErrCatalogFence) {
			code = "artifact_snapshot_changed"
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
		return WorkerResult{RetryAt: &next, ErrorCode: code, ErrorMessage: "媒体产物快照已变化或生成失败，将重新校验后重试"}
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
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Resolve filesystem identity outside the catalog transaction, including
		// after upstream downloads and immediately before each physical write.
		_, identity, err := canonicalProjectionRoot(policy.ProjectionRoot)
		if err != nil || identity != policy.ProjectionRootIdentity {
			return ErrCatalogFence
		}
		return s.catalogStore.readDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return s.validateArtifactBindingTx(tx, policy, &run, &claim, true) })
	}
	if err := check(); err != nil {
		return fail(err)
	}
	if row.State == "applying" {
		return s.finalizeBoundArtifacts(ctx, claim, run, policy, binding, fail)
	}
	root, err := (storagefs.LocalDriver{}).CanonicalizeRoot(policy.ProjectionRoot)
	if err != nil {
		return fail(err)
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
	run.ExpectedCount, run.WrittenCount, run.UpdatedCount, run.SkippedCount, run.FailedCount = 0, 0, 0, 0, 0
	process := func(write func(*artifactManifestIndex) (string, error)) error {
		if err := check(); err != nil {
			return err
		}
		manifest := newArtifactManifestIndex(8)
		manifest.physical = &artifactPhysicalExecution{ctx: ctx, permit: permit, policy: policy}
		manifest.catalogBindingID = policy.CatalogBindingID
		manifest.lazy = true
		manifest.beforeWrite = check
		manifest.reserve = func(artifact *models.MediaArtifact) error {
			return s.catalogArtifactWriteTx(ctx, func(tx *gorm.DB) error {
				if err := s.validateArtifactBindingTx(tx, policy, &run, &claim, true); err != nil {
					return err
				}
				return tx.Create(artifact).Error
			})
		}
		outcome, err := write(manifest)
		if err != nil {
			return err
		}
		if err := s.persistBoundArtifactManifest(ctx, claim, run, policy, manifest); err != nil {
			return err
		}
		run.ExpectedCount++
		switch outcome {
		case "written":
			run.WrittenCount++
		case "updated":
			run.UpdatedCount++
		case "skipped":
			run.SkippedCount++
		}
		processed := int64(run.ExpectedCount)
		return runtime.Heartbeat(nil, &processed, nil, nil, nil)
	}
	read := func(fn func(*CatalogReader) error) error {
		return s.catalogStore.ReadBoundCatalog(ctx, binding, "artifact", row.ID, func(reader *CatalogReader) error {
			if err := s.validateArtifactBindingTx(reader.tx, policy, &run, &claim, false); err != nil {
				return err
			}
			return fn(reader)
		})
	}
	if policy.STRMEnabled {
		var after uint
		for {
			var entries []models.MediaLibraryEntry
			if err := read(func(r *CatalogReader) error {
				return r.Entries().Where("library_id = ? AND id > ?", run.LibraryID, after).Order("id").Limit(CatalogBatchRows).Find(&entries).Error
			}); err != nil {
				return fail(err)
			}
			if len(entries) == 0 {
				break
			}
			for _, entry := range entries {
				if err := process(func(m *artifactManifestIndex) (string, error) { return s.writeSTRM(ctx, root, run, entry, m, verifier) }); err != nil {
					return fail(err)
				}
				after = entry.ID
			}
		}
		if policy.StorageType != models.StorageTypeLocal {
			after = 0
			for {
				var assets []models.MediaLibrarySourceAsset
				if err := read(func(r *CatalogReader) error {
					return r.SourceAssets().Where("library_id = ? AND active = ? AND id > ?", run.LibraryID, true, after).Order("id").Limit(CatalogBatchRows).Find(&assets).Error
				}); err != nil {
					return fail(err)
				}
				if len(assets) == 0 {
					break
				}
				for _, asset := range assets {
					if err := process(func(m *artifactManifestIndex) (string, error) {
						return s.writeSourceAsset(ctx, root, run, policy, asset, m)
					}); err != nil {
						return fail(err)
					}
					after = asset.ID
				}
			}
		}
	}
	if policy.Metadata {
		var after uint
		for {
			var records []models.MediaLibraryRecognition
			if err := read(func(r *CatalogReader) error {
				return r.Recognitions().Where("library_id = ? AND status = ? AND id > ?", run.LibraryID, mediaRecognitionStatusMatched, after).Order("id").Limit(25).Find(&records).Error
			}); err != nil {
				return fail(err)
			}
			if len(records) == 0 {
				break
			}
			for _, record := range records {
				var entries []models.MediaLibraryEntry
				if err := read(func(r *CatalogReader) error {
					return r.Entries().Where("library_id = ? AND recognition_id = ?", run.LibraryID, record.ID).Order("relative_path").Limit(1).Find(&entries).Error
				}); err != nil {
					return fail(err)
				}
				if err := process(func(m *artifactManifestIndex) (string, error) {
					return s.writeNFO(ctx, root, run, policy.TargetKind, record, entries, m)
				}); err != nil {
					return fail(err)
				}
				after = record.ID
			}
		}
	}
	// All physical writes and their manifests have succeeded before applied.
	if err := s.catalogArtifactWriteTx(ctx, func(tx *gorm.DB) error {
		if err := s.validateArtifactBindingTx(tx, policy, &run, &claim, true); err != nil {
			return err
		}
		now := time.Now().UTC()
		if err := tx.Model(&models.MediaArtifactRun{}).Where("id = ?", run.ID).Updates(map[string]any{"expected_count": run.ExpectedCount, "written_count": run.WrittenCount, "updated_count": run.UpdatedCount, "skipped_count": run.SkippedCount, "failed_count": 0, "updated_at": now}).Error; err != nil {
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
			if !policy.ScanPartial {
				if err := tx.Model(&models.MediaArtifact{}).Where("library_id = ? AND target_kind = ? AND id > ? AND catalog_binding_id <> ? AND active = ?", run.LibraryID, policy.TargetKind, row.FinalizeAfterID, row.ID, true).Order("id").Limit(CatalogBatchRows).Pluck("id", &ids).Error; err != nil {
					return err
				}
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
