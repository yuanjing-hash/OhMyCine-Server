package services

import (
	"errors"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// Match a remote asset by its stable identity before the path-keyed upsert.
// A renamed file keeps its catalog ID. Ambiguous identities remain conflicts;
// this is not a historical duplicate repair or a provider file operation.
func matchRenamedSourceAssetsTx(tx *gorm.DB, libraryID uint, existing []models.MediaLibrarySourceAsset, sources []medialibrary.SourceAsset) ([]models.MediaLibrarySourceAsset, error) {
	byProvider := make(map[string][]int)
	byPath := make(map[string]int)
	for i, row := range existing {
		byPath[row.RelativePath] = i
		if row.ProviderID != "" {
			byProvider[row.ProviderID] = append(byProvider[row.ProviderID], i)
		}
	}
	seen := make(map[string]bool)
	for _, source := range sources {
		if source.ProviderID == "" {
			continue
		}
		if seen[source.ProviderID] {
			return nil, errors.New("scan contains duplicate asset identity")
		}
		seen[source.ProviderID] = true
		indices := byProvider[source.ProviderID]
		if len(indices) == 0 {
			continue
		}
		if len(indices) != 1 {
			return nil, errors.New("catalog contains duplicate asset identity")
		}
		index := indices[0]
		asset := existing[index]
		if asset.RelativePath == source.RelativePath {
			continue
		}
		if occupant, ok := byPath[source.RelativePath]; ok && occupant != index {
			return nil, errors.New("asset rename target belongs to another identity")
		}
		if err := tx.Model(&models.MediaLibrarySourceAsset{}).Where("library_id = ? AND id = ? AND provider_id = ?", libraryID, asset.ID, source.ProviderID).Update("relative_path", source.RelativePath).Error; err != nil {
			return nil, err
		}
		delete(byPath, asset.RelativePath)
		byPath[source.RelativePath] = index
		existing[index].RelativePath = source.RelativePath
	}
	return existing, nil
}
