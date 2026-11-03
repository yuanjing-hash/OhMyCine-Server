package services

import (
	"bytes"

	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
)

func selectedOriginFixture(t *testing.T) (DownloadSourceInput, downloadSourceEnvelope, models.Site) {
	t.Helper()
	manifest, err := resourcecontent.ParseTorrent(inspectionTorrent("Show.S01E13.mkv", "Show.S01E15.mkv"), resourcecontent.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	file := manifest.Files[1]
	selection := resourcecontent.Selection{Version: resourcecontent.SelectionVersion, Identity: manifest.Identity, Files: []resourcecontent.SelectedFile{{Index: file.Index, Path: file.Path, Size: file.Size}}}
	site := models.Site{ID: 7, Kind: "pansou_tg", Revision: 9, Enabled: true}
	claim := siteResultClaim{ActorID: 3, SiteID: site.ID, SiteRevision: site.Revision, SourceKind: RouteSourceBT, PayloadKind: sitepkg.PayloadMagnet}
	input, err := applyContentSelection(claim, DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:" + manifest.Identity.V1}, ResolvedContentSelection{Manifest: &manifest, Torrent: &selection})
	if err != nil || input.selectionOrigin == nil {
		t.Fatal("missing private source authority", err)
	}
	normalized, _, err := normalizeDownloadSource(input, "Selected E15")
	if err != nil {
		t.Fatal(err)
	}
	return input, normalized, site
}

func TestSelectedOriginCannotBeForgedByKindOwnerSiteHashOrSelection(t *testing.T) {
	d := models.Downloader{Type: models.DownloaderTypePan115Offline}
	input, source, site := selectedOriginFixture(t)
	if err := validateSelectedSiteDownloadSource(d, source, input.selectionOrigin, 3, site); err != nil {
		t.Fatal("valid derived source refused", err)
	}
	if err := validateSelectedSiteDownloadSource(d, source, nil, 3, site); err == nil {
		t.Fatal("unproven raw PanSou torrent accepted")
	}
	for _, name := range []string{"owner", "site", "revision", "share-origin", "pt-origin", "original-kind", "wrong-hash", "changed-metainfo", "changed-selection", "missing-selection", "private-site", "disabled-site"} {
		t.Run(name, func(t *testing.T) {
			origin := *input.selectionOrigin
			copySource := source
			copySource.Torrent = bytes.Clone(source.Torrent)
			selection := *source.ContentSelection
			selection.Files = append([]resourcecontent.SelectedFile(nil), selection.Files...)
			copySource.ContentSelection = &selection
			copySite := site
			switch name {
			case "owner":
				origin.OwnerID++
			case "site":
				origin.SiteID++
			case "revision":
				copySite.Revision++
			case "share-origin":
				origin.SourceKind = RouteSource115Share
			case "pt-origin":
				origin.SourceKind = RouteSourcePT
			case "original-kind":
				origin.OriginalKind = downloadpkg.SourceTorrent
			case "wrong-hash":
				origin.Identity.V1 = "0000000000000000000000000000000000000000"
			case "changed-metainfo":
				copySource.Torrent = inspectionTorrent("Other.S01E15.mkv")
			case "changed-selection":
				copySource.ContentSelection.Files[0].Index = 0
			case "missing-selection":
				copySource.ContentSelection = nil
			case "private-site":
				copySite.Kind = "nexusphp"
			case "disabled-site":
				copySite.Enabled = false
			}
			if err := validateSelectedSiteDownloadSource(d, copySource, &origin, 3, copySite); err == nil {
				t.Fatalf("accepted %s", name)
			}
		})
	}
}

func TestSelectedOriginCreationRequiresActualPublicMagnetIdentity(t *testing.T) {
	input, source, site := selectedOriginFixture(t)
	manifest, err := resourcecontent.ParseTorrent(source.Torrent, resourcecontent.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{RouteSourcePT, RouteSource115Share, RouteSourceQuarkShare, ""} {
		claim := siteResultClaim{ActorID: 3, SiteID: site.ID, SiteRevision: site.Revision, SourceKind: kind, PayloadKind: sitepkg.PayloadMagnet}
		got, err := applyContentSelection(claim, DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:" + manifest.Identity.V1}, ResolvedContentSelection{Manifest: &manifest, Torrent: input.ContentSelection})
		if err != nil {
			t.Fatal(err)
		}
		if got.selectionOrigin != nil {
			t.Fatalf("non-public source %q minted authority", kind)
		}
	}
	claim := siteResultClaim{ActorID: 3, SiteID: site.ID, SiteRevision: site.Revision, SourceKind: RouteSourceBT, PayloadKind: sitepkg.PayloadMagnet}
	if _, err := applyContentSelection(claim, DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:0000000000000000000000000000000000000000"}, ResolvedContentSelection{Manifest: &manifest, Torrent: input.ContentSelection}); err == nil {
		t.Fatal("wrong original magnet identity accepted")
	}
}
