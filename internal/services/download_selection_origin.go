package services

import (
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

// selectedSourceOrigin separates trusted discovery origin from the executor's
// metainfo representation. Only applyContentSelection creates it after matching
// the resolved original magnet, manifest and exact file selection. It exists
// only during admission; accepted tasks already own their encrypted immutable
// source/selection digest and resume through the existing task/receipt path.
type selectedSourceOrigin struct {
	OwnerID, SiteID                           uint
	SiteRevision                              uint64
	SourceKind, OriginalKind, SelectionDigest string
	Identity                                  resourcecontent.Identity
}

func validateSelectedSiteDownloadSource(downloader models.Downloader, source downloadSourceEnvelope, origin *selectedSourceOrigin, ownerID uint, site models.Site) error {
	if origin == nil {
		return validateSiteDownloadSource(downloader, source.Kind, site)
	}
	if origin.OwnerID == 0 || origin.OwnerID != ownerID || origin.SiteID != site.ID || origin.SiteRevision == 0 || origin.SiteRevision != site.Revision || origin.SourceKind != RouteSourceBT || origin.OriginalKind != downloadpkg.SourceURL || source.Kind != downloadpkg.SourceTorrent || source.ContentSelection == nil || source.SelectionMode != "files" || origin.SelectionDigest == "" || origin.SelectionDigest != source.ContentSelectionDigest {
		return contentInspectionExpired()
	}
	manifest, err := downloadpkg.ValidateContentSource(downloadpkg.Source{Kind: source.Kind, Torrent: source.Torrent, ContentSelection: source.ContentSelection})
	if err != nil || manifest.Private || manifest.ValidateIdentity(origin.Identity) != nil || validateStoredContentSelection(source) != nil {
		return contentInspectionExpired()
	}
	// Recheck the current site's original source policy at both admission
	// boundaries. No raw torrent whitelist or client-provided source label.
	return validateSiteDownloadSource(downloader, origin.OriginalKind, site)
}
