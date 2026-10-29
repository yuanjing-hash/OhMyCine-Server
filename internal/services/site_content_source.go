package services

import (
	"bytes"

	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

// applyContentSelection replaces only acquisition material, after the ordinary
// Site handoff has revalidated its trusted origin and current native source.
// It preserves the entire original info dictionary and selects existing rows.
func applyContentSelection(claim siteResultClaim, source DownloadSourceInput, selected ResolvedContentSelection) (DownloadSourceInput, error) {
	if selected.Torrent != nil && selected.Manifest != nil {
		if err := selected.Torrent.ValidateAgainst(*selected.Manifest); err != nil {
			return source, contentInspectionExpired()
		}
		identity := selected.Manifest.Identity
		if len(source.Torrent) > 0 {
			actual, err := resourcecontent.ParseTorrent(source.Torrent, resourcecontent.Limits{})
			if err != nil || actual.ValidateIdentity(identity) != nil {
				return source, contentInspectionExpired()
			}
		} else {
			magnet, err := resourcecontent.ParseMagnet(source.URL)
			if err != nil || magnet.Identity.V1 != "" && magnet.Identity.V1 != identity.V1 || magnet.Identity.V2 != "" && magnet.Identity.V2 != identity.V2 {
				return source, contentInspectionExpired()
			}
		}
		verifiedAll := selected.AllFilesVerified && selected.OriginalMagnet != "" && source.Kind == downloadpkg.SourceURL && source.URL == selected.OriginalMagnet
		if !verifiedAll {
			source.Kind, source.URL = downloadpkg.SourceTorrent, ""
		}
		source.AllFilesVerified = verifiedAll
		source.Filename = selected.Manifest.Name + ".torrent"
		source.Torrent = bytes.Clone(selected.Manifest.MetainfoBytes)
		source.ContentSelection = selected.Torrent
		return source, nil
	}
	if selected.Share != nil && isCloudShareSource(source.Kind) {
		raw, provider, _, err := normalizeCloudShare(claim.TorrentID)
		if err != nil || provider+"_share" != source.Kind || source.URL != raw {
			return source, contentInspectionExpired()
		}
		source.ShareSelection = selected.Share
		return source, nil
	}
	return source, contentInspectionExpired()
}
