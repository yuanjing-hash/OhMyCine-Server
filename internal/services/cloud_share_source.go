package services

import (
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/pan115"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
)

func isCloudShareSource(kind string) bool {
	return kind == downloadpkg.SourcePan115Share || kind == downloadpkg.SourceQuarkShare
}
func isCloudShareDownloader(kind string) bool {
	return kind == models.DownloaderTypePan115Offline || kind == models.DownloaderTypeQuarkShare
}
func shareStorageType(kind string) string {
	if kind == models.DownloaderTypeQuarkShare {
		return models.StorageTypeQuark
	}
	return models.StorageTypePan115
}
func normalizeCloudShare(raw string) (normalized, provider, identity string, err error) {
	if normalized, identity, err = quark.NormalizeShareLink(raw, ""); err == nil {
		return normalized, "quark", identity, nil
	}
	normalized, identity, err = pan115.NormalizeShareLink(raw, "")
	return normalized, "115", identity, err
}
func claimCloudSource(claim siteResultClaim) string {
	if isCloudShareSource(claim.SourceKind) {
		return claim.SourceKind
	}
	// Old in-memory claims are accepted only if their saved URL itself proves
	// the legacy 115 source. A missing provider never silently authorizes Quark.
	if claim.SourceKind == "" && claim.CloudProvider == "" {
		if _, _, err := pan115.NormalizeShareLink(claim.TorrentID, ""); err == nil {
			return downloadpkg.SourcePan115Share
		}
	}
	return ""
}
