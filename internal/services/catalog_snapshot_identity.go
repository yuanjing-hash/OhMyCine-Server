package services

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

type CatalogIdentityRequest struct {
	Kind      string
	SourceKey string
	// ProviderID proves a rename of a currently effective entry/asset. It is
	// never an API identity and is not used for recognition-unit matching.
	ProviderID string
	// ExistingID must belong to the current source epoch and effective head.
	// Zero allocates a candidate-local identity, not a historical path alias.
	ExistingID uint
}

// Identity allocation has a different cost from fact append: it writes both
// stable anchors (and their legacy indexes) and lifetime mappings. Start with a
// conservative batch and adapt from observed transaction cost, not library size.
type catalogIdentityBatchBudget struct {
	mu   sync.Mutex
	rows int
}

func (b *catalogIdentityBatchBudget) limit() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rows == 0 {
		b.rows = 32
	}
	return b.rows
}

func (b *catalogIdentityBatchBudget) observe(rows int, elapsed time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rows == 0 {
		b.rows = 32
	}
	if elapsed > 80*time.Millisecond {
		b.rows = max(8, min(b.rows, rows)/2)
	} else if rows >= b.rows && elapsed < 30*time.Millisecond {
		b.rows = min(128, b.rows+8)
	}
}

// ResolveIdentities returns IDs in request order. It allocates only stable
// private anchors: no entry can become visible until a complete head publishes.
// Catalog identity is independent from duplicate-file/version classification.
func (s *CatalogSnapshotStore) ResolveIdentities(ctx context.Context, id, token string, requests []CatalogIdentityRequest) ([]uint, error) {
	if len(requests) == 0 || len(requests) > CatalogBatchRows {
		return nil, ErrCatalogBudget
	}
	var byteCount int
	for _, r := range requests {
		if r.SourceKey == "" || len(r.SourceKey) > 2048 || strings.IndexByte(r.SourceKey, 0) >= 0 {
			return nil, ErrCatalogInvalid
		}
		if len(r.ProviderID) > 2048 || strings.IndexByte(r.ProviderID, 0) >= 0 || (r.Kind == "recognition" && r.ProviderID != "") {
			return nil, ErrCatalogInvalid
		}
		byteCount += len(r.SourceKey) + len(r.ProviderID)
		if r.Kind != "entry" && r.Kind != "recognition" && r.Kind != "asset" {
			return nil, ErrCatalogInvalid
		}
	}
	if byteCount > CatalogBatchBytes {
		return nil, ErrCatalogBudget
	}
	result := make([]uint, 0, len(requests))
	for start := 0; start < len(requests); {
		end := min(start+s.identityBudget.limit(), len(requests))
		part, elapsed, err := s.resolveCatalogIdentityBatch(ctx, id, token, requests[start:end])
		s.identityBudget.observe(end-start, elapsed)
		if err != nil {
			return nil, err
		}
		result = append(result, part...)
		start = end
	}
	return result, nil
}

func (s *CatalogSnapshotStore) resolveCatalogIdentityBatch(ctx context.Context, id, token string, requests []CatalogIdentityRequest) ([]uint, time.Duration, error) {
	result := make([]uint, len(requests))
	var elapsed time.Duration
	byteCount := 0
	for _, request := range requests {
		byteCount += len(request.SourceKey) + len(request.ProviderID)
	}
	err := s.writeCatalogGrowthObserved(ctx, int64(byteCount+len(requests)*512), func(tx *gorm.DB) error {
		candidate, err := catalogOwnedCandidate(tx, id, token, "building")
		if err != nil {
			return err
		}
		var head models.CatalogHead
		if err := tx.First(&head, "library_id=?", candidate.LibraryID).Error; err != nil {
			return err
		}
		reader, err := PinCatalogTx(tx, []uint{candidate.LibraryID})
		if err != nil {
			return err
		}
		// Resolve each entity kind in bounded sets. Never query the complete
		// effective catalog once per file while reserving SQLite's writer.
		for _, kind := range []string{"entry", "recognition", "asset"} {
			indexes := make([]int, 0)
			keys, existingIDs := make([]string, 0), make([]uint, 0)
			for i, request := range requests {
				if request.Kind != kind {
					continue
				}
				indexes = append(indexes, i)
				keys = append(keys, catalogIdentityAllocationKey(candidate.ID, head.Mode, request))
				if request.ExistingID != 0 {
					existingIDs = append(existingIDs, request.ExistingID)
				}
			}
			if len(indexes) == 0 {
				continue
			}
			var identities []models.CatalogIdentity
			// Keep both indexed key domains separate. OR across source_key and
			// anchor_id makes SQLite scan the entire library prefix per batch.
			if err := tx.Where("library_id=? AND source_epoch=? AND entity_kind=? AND source_key IN ?", candidate.LibraryID, candidate.SourceEpoch, kind, keys).Find(&identities).Error; err != nil {
				return err
			}
			if len(existingIDs) > 0 {
				var existing []models.CatalogIdentity
				if err := tx.Where("library_id=? AND source_epoch=? AND entity_kind=? AND anchor_id IN ?", candidate.LibraryID, candidate.SourceEpoch, kind, existingIDs).Find(&existing).Error; err != nil {
					return err
				}
				identities = append(identities, existing...)
			}
			byKey, byID := make(map[string]uint), make(map[uint]bool)
			for _, row := range identities {
				byKey[row.SourceKey], byID[row.AnchorID] = row.AnchorID, true
			}
			evidence, err := catalogIdentityEvidence(reader, kind, existingIDs)
			if err != nil {
				return err
			}
			pending := make([]models.CatalogIdentity, 0)
			newKeys := make([]string, 0)
			newKeySet := make(map[string]bool)
			for j, i := range indexes {
				r, key := requests[i], keys[j]
				if r.ExistingID != 0 {
					fact, ok := evidence[r.ExistingID]
					if !ok || (fact.SourceKey != r.SourceKey && (kind == "recognition" || r.ProviderID == "" || fact.ProviderID != r.ProviderID)) {
						return ErrCatalogInvalid
					}
					if head.Mode == "converting" {
						if fact.SourceKey != r.SourceKey {
							return ErrCatalogInvalid
						}
						if mapped := byKey[key]; mapped != 0 && mapped != r.ExistingID {
							return ErrCatalogInvalid
						}
						if byKey[key] == 0 {
							pending = append(pending, models.CatalogIdentity{LibraryID: candidate.LibraryID, SourceEpoch: candidate.SourceEpoch, EntityKind: kind, SourceKey: key, AnchorID: r.ExistingID, CreatedAt: time.Now().UTC()})
							byKey[key] = r.ExistingID
						}
					} else if head.Mode != "versioned" || !byID[r.ExistingID] {
						return ErrCatalogInvalid
					}
					result[i] = r.ExistingID
					continue
				}
				// Conversion cannot add visible legacy anchors, even on retry.
				if head.Mode != "versioned" {
					return ErrCatalogInvalid
				}
				if byKey[key] == 0 && !newKeySet[key] {
					newKeys = append(newKeys, key)
					newKeySet[key] = true
				}
			}
			allocated, err := allocateCatalogAnchors(tx, candidate.LibraryID, kind, len(newKeys))
			if err != nil {
				return err
			}
			for i, key := range newKeys {
				byKey[key] = allocated[i]
				pending = append(pending, models.CatalogIdentity{LibraryID: candidate.LibraryID, SourceEpoch: candidate.SourceEpoch, EntityKind: kind, SourceKey: key, AnchorID: allocated[i], CreatedAt: time.Now().UTC()})
			}
			if len(pending) > 0 {
				if err := tx.CreateInBatches(pending, 100).Error; err != nil {
					return err
				}
			}
			for j, i := range indexes {
				if requests[i].ExistingID == 0 {
					result[i] = byKey[keys[j]]
				}
			}
		}
		return nil
	}, func(duration time.Duration) { elapsed = duration })
	if err != nil {
		return nil, elapsed, err
	}
	return result, elapsed, nil
}

func catalogIdentityAllocationKey(candidateID, mode string, request CatalogIdentityRequest) string {
	if mode == "converting" {
		return request.SourceKey
	}
	return "allocation:" + candidateID + ":" + catalogTokenHash(request.Kind+"\x00"+request.SourceKey)
}

type catalogIdentitySource struct {
	ID         uint
	SourceKey  string
	ProviderID string
}

func catalogIdentityEvidence(reader *CatalogReader, kind string, ids []uint) (map[uint]catalogIdentitySource, error) {
	result := make(map[uint]catalogIdentitySource)
	if len(ids) == 0 {
		return result, nil
	}
	query, columns := reader.entryIdentities(), "id, relative_path AS source_key, provider_id"
	if kind == "recognition" {
		query, columns = reader.Recognitions(), "id, source_key, '' AS provider_id"
	}
	if kind == "asset" {
		query = reader.SourceAssets()
	}
	var rows []catalogIdentitySource
	if err := query.Select(columns).Where("id IN ?", ids).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.ID] = row
	}
	return result, nil
}

func allocateCatalogAnchors(tx *gorm.DB, libraryID uint, kind string, count int) ([]uint, error) {
	ids := make([]uint, count)
	if count == 0 {
		return ids, nil
	}
	// Locators belong solely to facts; these private FK anchors never change.
	switch kind {
	case "entry":
		rows := make([]models.MediaLibraryEntry, count)
		for i := range rows {
			rows[i] = models.MediaLibraryEntry{LibraryID: libraryID, RelativePath: ".omc-catalog-anchor/" + uuid.NewString(), MatchStatus: "pending"}
		}
		if err := tx.CreateInBatches(&rows, 100).Error; err != nil {
			return nil, err
		}
		for i := range rows {
			ids[i] = rows[i].ID
		}
	case "recognition":
		var library models.MediaLibrary
		if err := tx.Select("id,profile_id").First(&library, libraryID).Error; err != nil {
			return nil, err
		}
		rows := make([]models.MediaLibraryRecognition, count)
		for i := range rows {
			rows[i] = models.MediaLibraryRecognition{LibraryID: libraryID, ProfileID: library.ProfileID, SourceKey: ".omc-catalog-anchor/" + uuid.NewString(), Status: "pending", MetadataJSON: "{}"}
		}
		if err := tx.CreateInBatches(&rows, 100).Error; err != nil {
			return nil, err
		}
		for i := range rows {
			ids[i] = rows[i].ID
		}
	case "asset":
		rows := make([]models.MediaLibrarySourceAsset, count)
		for i := range rows {
			rows[i] = models.MediaLibrarySourceAsset{LibraryID: libraryID, RelativePath: ".omc-catalog-anchor/" + uuid.NewString(), Active: false}
		}
		if err := tx.CreateInBatches(&rows, 100).Error; err != nil {
			return nil, err
		}
		for i := range rows {
			ids[i] = rows[i].ID
		}
		// GORM fills a bool tagged default:true even when the struct says false.
		// Keep these FK-only anchors inactive before the allocation commits.
		if err := tx.Model(&models.MediaLibrarySourceAsset{}).Where("id IN ?", ids).UpdateColumn("active", false).Error; err != nil {
			return nil, err
		}
	default:
		return nil, ErrCatalogInvalid
	}
	return ids, nil
}
