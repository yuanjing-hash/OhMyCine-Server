package services

import (
	"fmt"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"path/filepath"
	"strings"
)

// A complete cloud inventory authorizes a local health audit, not regeneration.
// Rebind healthy managed artifacts to preserve them in the existing manifest
// difference cleanup; only missing, damaged or source-changed items reach I/O.
func (s *MediaArtifactService) filterLegacyFullArtifactWork(root string, run models.MediaArtifactRun, policy mediaArtifactPolicy, entries []models.MediaLibraryEntry, recognitions []models.MediaLibraryRecognition, assets []models.MediaLibrarySourceAsset, by map[uint][]models.MediaLibraryEntry, manifest *artifactManifestIndex, verifier signedArtifactVerifier) ([]models.MediaLibraryEntry, []models.MediaLibraryRecognition, []models.MediaLibrarySourceAsset) {
	sources := map[string][]models.MediaArtifact{}
	records := map[uint][]models.MediaArtifact{}
	for _, row := range manifest.rows {
		sources[row.SourceIdentity] = append(sources[row.SourceIdentity], row)
		var id uint
		if _, err := fmt.Sscanf(row.SourceIdentity, "recognition:%d", &id); err == nil {
			records[id] = append(records[id], row)
		}
	}
	retain := func(rows []models.MediaArtifact, provider string) {
		for _, row := range rows {
			row.RunID, row.Active = run.ID, true
			if provider != "" {
				row.ProviderItemID = provider
			}
			key := artifactManifestKey(row.TargetKind, row.RelativePath)
			manifest.rows[key] = row
			manifest.dirty[key] = struct{}{}
		}
	}
	pendingEntries := make([]models.MediaLibraryEntry, 0)
	for _, entry := range entries {
		rows := sources[fmt.Sprintf("entry:%d", entry.ID)]
		healthy, _ := s.catalogSTRMArtifactsHealthy(root, entry, rows, verifier)
		if policy.STRMEnabled && healthy {
			// Recover lease metadata from the verified file without rewriting it.
			for i := range rows {
				if rows[i].ContentExpiresAt == nil || rows[i].ContentFormatVersion == "" {
					target := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(rows[i].RelativePath, "/")))
					if _, inspected, ok := s.reusableSTRM(target, rows[i], verifier); ok {
						rows[i].ContentExpiresAt = &inspected.ExpiresAt
						rows[i].ContentFormatVersion = inspected.FormatVersion
					}
				}
			}
			retain(rows, entry.ProviderID)
		} else {
			pendingEntries = append(pendingEntries, entry)
		}
	}
	pendingRecords := make([]models.MediaLibraryRecognition, 0)
	for _, record := range recognitions {
		rows := records[record.ID]
		healthy, _ := s.fullRecognitionArtifactsHealthy(root, record, by[record.ID], rows)
		if healthy {
			retain(rows, "")
		} else {
			pendingRecords = append(pendingRecords, record)
		}
	}
	pendingAssets := make([]models.MediaLibrarySourceAsset, 0)
	for _, asset := range assets {
		rows := sources[fmt.Sprintf("asset:%d", asset.ID)]
		healthy := len(rows) == 1 && rows[0].Managed && rows[0].Status == models.MediaArtifactStatusCompleted && rows[0].RelativePath == asset.RelativePath && rows[0].SourceFingerprint == artifactAssetSourceFingerprint(asset) && artifactFileMatches(root, rows[0])
		if healthy {
			retain(rows, asset.ProviderID)
		} else {
			pendingAssets = append(pendingAssets, asset)
		}
	}
	return pendingEntries, pendingRecords, pendingAssets
}
