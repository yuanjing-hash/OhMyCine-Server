package services

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// This explicit private codec intentionally does not inherit API json:"-"
// tags. Structural conversion fails compilation if the manifest gains fields.
type catalogArtifactPrivateMetadata struct {
	CatalogBindingID     string
	ID                   uint
	OpaqueID             string
	RunID                string
	LibraryID            uint
	SourceIdentity       string
	ProviderItemID       string
	ProviderParentID     string
	Kind                 string
	TargetKind           string
	RelativePath         string
	ContentFingerprint   string
	ContentExpiresAt     *time.Time
	ContentFormatVersion string
	TargetProviderID     string
	Managed              bool
	Active               bool
	Status               string
	ErrorCode            string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type catalogArtifactMetadataEnvelope struct {
	Version  int
	Artifact catalogArtifactPrivateMetadata
}

func encodeCatalogArtifactMetadata(row models.MediaArtifact) (string, error) {
	raw, err := json.Marshal(catalogArtifactMetadataEnvelope{1, catalogArtifactPrivateMetadata(row)})
	return string(raw), err
}

func decodeCatalogArtifactMetadata(raw string) (models.MediaArtifact, error) {
	var value catalogArtifactMetadataEnvelope
	if len(raw) > 64*1024 || json.Unmarshal([]byte(raw), &value) != nil || value.Version != 1 {
		return models.MediaArtifact{}, ErrCatalogInvalid
	}
	return models.MediaArtifact(value.Artifact), nil
}

type CatalogArtifactWriteInput struct {
	BeforeArtifact, AfterArtifact       models.MediaArtifact
	RootIdentity                        string
	BeforeExists                        bool
	BeforeFingerprint, AfterFingerprint string
	BeforeSize, AfterSize               int64
}

type CatalogArtifactObservation struct {
	RootIdentity string
	Exists       bool
	Fingerprint  string
	Size         int64
}

func catalogArtifactPermitTx(tx *gorm.DB, permit CatalogPhysicalWritePermit) (models.CatalogPhysicalWrite, models.MediaArtifactRun, error) {
	proof := permit.evidence
	var current models.CatalogPhysicalWrite
	var run models.MediaArtifactRun
	if err := requireCatalogTransaction(tx); err != nil {
		return current, run, err
	}
	if proof.ID == 0 || proof.OwnerKind != CatalogPhysicalArtifact || proof.ArtifactReceiptVersion != 1 {
		return current, run, ErrCatalogFence
	}
	if err := requireCatalogPhysicalPermitRuntimeTx(tx, proof); err != nil {
		return current, run, err
	}
	if err := tx.First(&current, proof.ID).Error; err != nil {
		return current, run, err
	}
	if current.Revision != proof.Revision || current.RuntimeID != proof.RuntimeID || current.LibraryID != proof.LibraryID || current.OwnerKind != proof.OwnerKind || current.OwnerID != proof.OwnerID || current.JobID != proof.JobID || current.JobLeaseHash != proof.JobLeaseHash || current.OwnerDigest != proof.OwnerDigest || current.ClaimDigest != proof.ClaimDigest || current.ArtifactReceiptVersion != 1 || (current.State != "entered" && current.State != "quiescent") {
		return current, run, ErrCatalogFence
	}
	if err := tx.First(&run, "id=?", proof.OwnerID).Error; err != nil {
		return current, run, err
	}
	if run.LibraryID != proof.LibraryID || run.JobID == nil || *run.JobID != proof.JobID || catalogPhysicalDigest(catalogPhysicalDigest(run.PolicyJSON, strconv.FormatUint(run.Generation, 10)), uintID(run.LibraryID), "0") != proof.OwnerDigest {
		return current, run, ErrCatalogFence
	}
	library, storage, profile, err := catalogConversionContextTx(tx, proof.LibraryID)
	if err != nil {
		return current, run, err
	}
	var epoch uint64
	if err := tx.Model(&models.CatalogHead{}).Select("source_epoch").Where("library_id=?", proof.LibraryID).Scan(&epoch).Error; err != nil {
		return current, run, err
	}
	if proof.SourceFingerprint != catalogSourceFingerprint(library, storage) || proof.ConfigFingerprint != catalogConfigFingerprint(library, storage, profile) || proof.SourceEpoch != epoch || current.SourceFingerprint != proof.SourceFingerprint || current.ConfigFingerprint != proof.ConfigFingerprint || current.SourceEpoch != proof.SourceEpoch {
		return current, run, ErrCatalogFence
	}
	return current, run, nil
}

func PrepareCatalogArtifactWriteTx(tx *gorm.DB, permit CatalogPhysicalWritePermit, input CatalogArtifactWriteInput) (models.CatalogArtifactWriteReceipt, error) {
	var receipt models.CatalogArtifactWriteReceipt
	proof, run, err := catalogArtifactPermitTx(tx, permit)
	if err != nil {
		return receipt, err
	}
	if proof.State != "entered" {
		return receipt, ErrCatalogFence
	}
	if err := catalogCheckJob(tx, models.CatalogSnapshot{JobID: &proof.JobID, JobLeaseHash: proof.JobLeaseHash}, time.Now().UTC()); err != nil {
		return receipt, err
	}
	b, a := input.BeforeArtifact, input.AfterArtifact
	var policy mediaArtifactPolicy
	if json.Unmarshal([]byte(run.PolicyJSON), &policy) != nil || policy.LibraryID != run.LibraryID || policy.Generation != run.Generation || policy.ProjectionRootIdentity != input.RootIdentity || policy.TargetKind != a.TargetKind {
		return receipt, ErrCatalogFence
	}
	if b.ID == 0 || a.ID != b.ID || a.OpaqueID != b.OpaqueID || a.LibraryID != proof.LibraryID || b.LibraryID != proof.LibraryID || a.RunID != run.ID || a.TargetKind != b.TargetKind || a.RelativePath != b.RelativePath || a.Kind != b.Kind || a.TargetKind == "" || a.RelativePath == "" || input.RootIdentity == "" || len(input.RootIdentity) > 4096 || !a.Managed || a.ContentFingerprint != input.AfterFingerprint || len(input.AfterFingerprint) != 64 || input.BeforeSize < 0 || input.AfterSize < 0 || input.AfterSize > 32*1024*1024 || (input.BeforeExists && len(input.BeforeFingerprint) != 64) || (!input.BeforeExists && (input.BeforeFingerprint != "" || input.BeforeSize != 0)) {
		return receipt, ErrCatalogInvalid
	}
	if input.BeforeExists && b.Status == models.MediaArtifactStatusCompleted && b.ContentFingerprint != input.BeforeFingerprint {
		return receipt, catalogPhysicalUnsettledError()
	}
	beforeJSON, err := encodeCatalogArtifactMetadata(b)
	if err != nil {
		return receipt, err
	}
	afterJSON, err := encodeCatalogArtifactMetadata(a)
	if err != nil {
		return receipt, err
	}
	var manifest models.MediaArtifact
	if err := tx.First(&manifest, b.ID).Error; err != nil {
		return receipt, err
	}
	actualJSON, err := encodeCatalogArtifactMetadata(manifest)
	if err != nil {
		return receipt, err
	}
	if actualJSON != beforeJSON {
		return receipt, ErrCatalogFence
	}
	var existing models.CatalogArtifactWriteReceipt
	err = tx.Where("run_id=? AND target_kind=? AND relative_path=?", run.ID, a.TargetKind, a.RelativePath).First(&existing).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return receipt, err
	}
	if existing.ID != 0 && (existing.Phase != "reconciled_before" && existing.Phase != "reconciled_after" || existing.Revision >= math.MaxInt64 || existing.PhysicalWriteID != proof.ID || existing.ArtifactID != a.ID) {
		return receipt, catalogPhysicalUnsettledError()
	}
	var unresolved uint64
	if err := tx.Model(&models.CatalogArtifactWriteReceipt{}).Select("id").Where("library_id=? AND target_kind=? AND relative_path=? AND phase IN ('prepared','conflict')", proof.LibraryID, a.TargetKind, a.RelativePath).Limit(1).Scan(&unresolved).Error; err != nil {
		return receipt, err
	}
	if unresolved != 0 {
		return receipt, catalogPhysicalUnsettledError()
	}
	if err := assertArtifactCleanupTargetResolvedTx(tx, a); err != nil {
		return receipt, err
	}
	now := time.Now().UTC()
	receipt = models.CatalogArtifactWriteReceipt{ID: existing.ID, LibraryID: proof.LibraryID, RunID: run.ID, ArtifactID: a.ID, PhysicalWriteID: proof.ID, PermitRevision: proof.Revision, Revision: existing.Revision + 1, TargetKind: a.TargetKind, RelativePath: a.RelativePath, RootIdentity: input.RootIdentity, PolicyDigest: catalogTokenHash(run.PolicyJSON), SourceEpoch: proof.SourceEpoch, SourceFingerprint: proof.SourceFingerprint, ConfigFingerprint: proof.ConfigFingerprint, BeforeExists: input.BeforeExists, BeforeFingerprint: input.BeforeFingerprint, AfterFingerprint: input.AfterFingerprint, BeforeSize: input.BeforeSize, AfterSize: input.AfterSize, BeforeArtifactJSON: beforeJSON, AfterArtifactJSON: afterJSON, Phase: "prepared", CreatedAt: now, UpdatedAt: now}
	if existing.ID == 0 {
		err := tx.Create(&receipt).Error
		return receipt, err
	}
	receipt.CreatedAt = existing.CreatedAt
	result := tx.Model(&models.CatalogArtifactWriteReceipt{}).Where("id=? AND revision=? AND phase=?", existing.ID, existing.Revision, existing.Phase).Select("*").Updates(&receipt)
	if result.Error != nil {
		return receipt, result.Error
	}
	if result.RowsAffected != 1 {
		return receipt, ErrCatalogFence
	}
	return receipt, nil
}

// Reconcile observes only; it never rewrites or deletes a physical file. The
// caller must commit conflict evidence (a conflict is returned in the reloaded
// receipt phase, not as an error that would roll back the containing writer).
func ReconcileCatalogArtifactWriteTx(tx *gorm.DB, permit CatalogPhysicalWritePermit, expected models.CatalogArtifactWriteReceipt, observed CatalogArtifactObservation) error {
	proof, run, err := catalogArtifactPermitTx(tx, permit)
	if err != nil {
		return err
	}
	var receipt models.CatalogArtifactWriteReceipt
	if err := tx.First(&receipt, expected.ID).Error; err != nil {
		return err
	}
	if receipt.Phase != expected.Phase || receipt.Revision >= math.MaxInt64 {
		return ErrCatalogFence
	}
	if receipt.Revision != expected.Revision || receipt.PhysicalWriteID != proof.ID || receipt.LibraryID != proof.LibraryID || receipt.RunID != proof.OwnerID || receipt.PermitRevision > proof.Revision || receipt.PolicyDigest != catalogTokenHash(run.PolicyJSON) || receipt.SourceEpoch != proof.SourceEpoch || receipt.SourceFingerprint != proof.SourceFingerprint || receipt.ConfigFingerprint != proof.ConfigFingerprint {
		return ErrCatalogFence
	}
	phase := "conflict"
	metadata := ""
	if observed.RootIdentity == receipt.RootIdentity {
		if observed.Exists && observed.Fingerprint == receipt.AfterFingerprint && observed.Size == receipt.AfterSize {
			phase, metadata = "reconciled_after", receipt.AfterArtifactJSON
		} else if observed.Exists == receipt.BeforeExists && observed.Fingerprint == receipt.BeforeFingerprint && observed.Size == receipt.BeforeSize {
			phase, metadata = "reconciled_before", receipt.BeforeArtifactJSON
		}
	}
	var current models.MediaArtifact
	if err := tx.First(&current, receipt.ArtifactID).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	} else if errors.Is(err, gorm.ErrRecordNotFound) {
		phase = "conflict"
	}
	currentJSON, err := encodeCatalogArtifactMetadata(current)
	if err != nil {
		return err
	}
	if currentJSON != receipt.BeforeArtifactJSON && currentJSON != receipt.AfterArtifactJSON {
		phase = "conflict"
	}
	if phase != "conflict" {
		row, err := decodeCatalogArtifactMetadata(metadata)
		if err != nil {
			return err
		}
		if row.ID != receipt.ArtifactID || row.LibraryID != receipt.LibraryID || row.TargetKind != receipt.TargetKind || row.RelativePath != receipt.RelativePath {
			return ErrCatalogFence
		}
		// UpdateColumns preserves the recorded timestamps and writes zero values.
		result := tx.Model(&models.MediaArtifact{}).Where("id=?", row.ID).Select("*").UpdateColumns(&row)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrCatalogFence
		}
	}
	code := ""
	if phase == "conflict" {
		code = "artifact_write_reconciliation_conflict"
	}
	result := tx.Model(&models.CatalogArtifactWriteReceipt{}).Where("id=? AND revision=? AND phase=?", receipt.ID, receipt.Revision, receipt.Phase).Updates(map[string]any{"revision": receipt.Revision + 1, "phase": phase, "error_code": code, "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrCatalogFence
	}
	return nil
}

// Bookkeeping-only settlement after every physical call returned. This does
// not publish catalog/artifact readiness and does not need a still-live lease:
// the original unforgeable runtime permit and exact physical receipts suffice.
func SettleSupersededCatalogArtifactTx(tx *gorm.DB, permit CatalogPhysicalWritePermit) error {
	proof, run, err := catalogArtifactPermitTx(tx, permit)
	if err != nil {
		return err
	}
	if proof.State != "quiescent" {
		return ErrCatalogFence
	}
	if (run.Status != models.MediaArtifactStatusSuperseded && run.Status != models.MediaArtifactStatusFailed) || run.FinishedAt == nil || run.CleanupStatus != models.MediaArtifactCleanupSkipped {
		return ErrCatalogFence
	}
	if err := assertCatalogArtifactReceiptsResolvedTx(tx, proof); err != nil {
		return err
	}
	now := time.Now().UTC()
	result := tx.Model(&models.CatalogPhysicalWrite{}).Where("id=? AND revision=? AND state='quiescent' AND runtime_id=?", proof.ID, proof.Revision, proof.RuntimeID).Updates(map[string]any{"state": "settled", "settled_at": now, "updated_at": now})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrCatalogFence
	}
	return nil
}

func assertCatalogArtifactReceiptsResolvedTx(tx *gorm.DB, proof models.CatalogPhysicalWrite) error {
	var unresolved uint64
	if err := tx.Model(&models.CatalogArtifactWriteReceipt{}).Select("id").Where("physical_write_id=? AND phase IN ('prepared','conflict')", proof.ID).Limit(1).Scan(&unresolved).Error; err != nil {
		return err
	}
	if unresolved != 0 {
		return catalogPhysicalUnsettledError()
	}
	var cleanup uint
	if err := tx.Model(&models.MediaArtifact{}).Select("id").Where("library_id=? AND managed=1 AND status='cleanup'", proof.LibraryID).Limit(1).Scan(&cleanup).Error; err != nil {
		return err
	}
	if cleanup != 0 {
		return catalogPhysicalUnsettledError()
	}
	// A claim is independent recovery evidence, even if a corrupted/legacy
	// manifest no longer carries its cleanup status. Never settle over it.
	if err := tx.Model(&models.CatalogArtifactCleanupClaim{}).Select("artifact_id").Where("library_id = ?", proof.LibraryID).Limit(1).Scan(&cleanup).Error; err != nil {
		return err
	}
	if cleanup != 0 {
		return catalogPhysicalUnsettledError()
	}
	return nil
}
