package services

import (
	"context"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// Private durable work input, never an API payload. Head identifies the exact
// publication; references prevent GC while short page transactions come and go.
type CatalogSnapshotBinding struct {
	Head   models.CatalogHead
	Layers []CatalogBindingLayer
}

type CatalogBindingLayer struct {
	SnapshotID string `json:"snapshot_id"`
	Rank       int    `json:"rank"`
}

func CaptureCatalogBindingTx(tx *gorm.DB, libraryID uint, ownerKind, ownerID string) (CatalogSnapshotBinding, error) {
	reader, err := PinCatalogTx(tx, []uint{libraryID})
	if err != nil {
		return CatalogSnapshotBinding{}, err
	}
	head, ok := reader.Head(libraryID)
	if !ok || head.Mode != "versioned" {
		return CatalogSnapshotBinding{}, ErrCatalogInvalid
	}
	binding := CatalogSnapshotBinding{Head: head}
	for _, layer := range reader.layers {
		if err := AcquireCatalogReferenceTx(tx, layer.SnapshotID, ownerKind, ownerID); err != nil {
			return CatalogSnapshotBinding{}, err
		}
		binding.Layers = append(binding.Layers, CatalogBindingLayer{SnapshotID: layer.SnapshotID, Rank: layer.Rank})
	}
	return binding, nil
}

// PinBoundCatalogTx checks the immutable source and the durable owner references.
// Current source/config authorization remains the caller's responsibility; this
// intentionally does not substitute a newer head during a long-running job.
func PinBoundCatalogTx(tx *gorm.DB, binding CatalogSnapshotBinding, ownerKind, ownerID string) (*CatalogReader, error) {
	if err := requireCatalogTransaction(tx); err != nil {
		return nil, err
	}
	h := binding.Head
	if err := requireMediaLibraryNotRetiringTx(tx, h.LibraryID); err != nil {
		return nil, err
	}
	if h.LibraryID == 0 || h.Mode != "versioned" || len(binding.Layers) == 0 || len(binding.Layers) > CatalogMaxDeltas+1 || ownerKind == "" || ownerID == "" {
		return nil, ErrCatalogInvalid
	}
	ids := make([]string, 0, len(binding.Layers))
	seen := make(map[string]bool)
	for i, layer := range binding.Layers {
		if layer.Rank != i || layer.SnapshotID == "" || seen[layer.SnapshotID] {
			return nil, ErrCatalogInvalid
		}
		seen[layer.SnapshotID] = true
		ids = append(ids, layer.SnapshotID)
	}
	var snapshots []models.CatalogSnapshot
	if err := tx.Where("id IN ?", ids).Find(&snapshots).Error; err != nil {
		return nil, err
	}
	var refs []models.CatalogSnapshotReference
	if err := tx.Where("snapshot_id IN ? AND owner_kind = ? AND owner_id = ?", ids, ownerKind, ownerID).Find(&refs).Error; err != nil {
		return nil, err
	}
	if len(snapshots) != len(ids) || len(refs) != len(ids) {
		return nil, ErrCatalogFence
	}
	byID := make(map[string]models.CatalogSnapshot, len(snapshots))
	for _, snapshot := range snapshots {
		byID[snapshot.ID] = snapshot
	}
	r := &CatalogReader{tx: tx, heads: map[uint]models.CatalogHead{h.LibraryID: h}, privateLayers: true}
	for _, layer := range binding.Layers {
		s := byID[layer.SnapshotID]
		if s.State != "published" || s.LibraryID != h.LibraryID || s.SourceEpoch != h.SourceEpoch || s.SourceFingerprint != h.SourceFingerprint || (layer.Rank == 0 && s.Kind != "base") || (layer.Rank > 0 && s.Kind != "delta") || s.PublishedRevision > h.Revision {
			return nil, ErrCatalogFence
		}
		r.layers = append(r.layers, models.CatalogHeadLayer{LibraryID: h.LibraryID, Rank: layer.Rank, SnapshotID: layer.SnapshotID})
	}
	return r, nil
}

func (s *CatalogSnapshotStore) ReadBoundCatalog(ctx context.Context, binding CatalogSnapshotBinding, ownerKind, ownerID string, read func(*CatalogReader) error) error {
	if s == nil || s.readDB == nil || read == nil {
		return ErrCatalogInvalid
	}
	return s.readDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		reader, err := PinBoundCatalogTx(tx, binding, ownerKind, ownerID)
		if err != nil {
			return err
		}
		return read(reader)
	})
}

func ReleaseCatalogBindingTx(tx *gorm.DB, binding CatalogSnapshotBinding, ownerKind, ownerID string) error {
	if len(binding.Layers) == 0 || len(binding.Layers) > CatalogMaxDeltas+1 {
		return ErrCatalogInvalid
	}
	for _, layer := range binding.Layers {
		if err := ReleaseCatalogReferenceTx(tx, layer.SnapshotID, ownerKind, ownerID); err != nil {
			return err
		}
	}
	return nil
}
