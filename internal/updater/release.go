package updater

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/buildinfo"
)

type Channel string

const (
	ChannelBeta   Channel = "beta"
	ChannelStable Channel = "stable"
)

func (c Channel) Valid() bool { return c == ChannelBeta || c == ChannelStable }

type Asset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"browser_download_url"`
	Size        int64  `json:"size"`
}

type Release struct {
	TagName     string    `json:"tag_name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []Asset   `json:"assets"`
}

type SelectedRelease struct {
	Version      buildinfo.SemanticVersion
	TagName      string
	Prerelease   bool
	Archive      Asset
	Checksum     Asset
	PublishedAt  time.Time
	PlatformName string
}

type PlatformAssets struct {
	Archive   string
	Checksum  string
	TopLevel  string
	Binary    string
	Extension string
}

func AssetNames(version, goos, goarch string) (PlatformAssets, error) {
	parsed, err := buildinfo.ParseVersion(version)
	if err != nil || parsed.String() != version {
		return PlatformAssets{}, coded(CodeInvalidRelease, errors.New("release version is invalid"))
	}
	if goarch != "amd64" || (goos != "windows" && goos != "linux") {
		return PlatformAssets{}, coded(CodeUnsupportedPlatform, errors.New("self-update is not supported on this platform"))
	}
	platform := goos + "-x64"
	top := fmt.Sprintf("OhMyCine-Server-v%s-%s", version, platform)
	assets := PlatformAssets{
		Checksum: fmt.Sprintf("OhMyCine-Server-v%s-SHA256SUMS.txt", version),
		TopLevel: top,
		Binary:   "ohmycine-server",
	}
	if goos == "windows" {
		assets.Binary += ".exe"
		assets.Extension = ".zip"
	} else {
		assets.Extension = ".tar.gz"
	}
	assets.Archive = top + assets.Extension
	return assets, nil
}

func SelectLatest(releases []Release, channel Channel, goos, goarch string) (SelectedRelease, error) {
	if !channel.Valid() {
		return SelectedRelease{}, coded(CodeInvalidChannel, errors.New("unknown update channel"))
	}
	var selected *SelectedRelease
	for _, release := range releases {
		if release.Draft || (channel == ChannelStable && release.Prerelease) {
			continue
		}
		candidate, err := validateRelease(release, goos, goarch)
		if err != nil {
			continue
		}
		if selected == nil || candidate.Version.Compare(selected.Version) > 0 {
			copy := candidate
			selected = &copy
		}
	}
	if selected == nil {
		return SelectedRelease{}, coded(CodeNoRelease, errors.New("no valid release is available"))
	}
	return *selected, nil
}

func ValidateRelease(release Release, goos, goarch string) (SelectedRelease, error) {
	if release.Draft {
		return SelectedRelease{}, coded(CodeInvalidRelease, errors.New("draft releases are not installable"))
	}
	return validateRelease(release, goos, goarch)
}

func validateRelease(release Release, goos, goarch string) (SelectedRelease, error) {
	if !strings.HasPrefix(release.TagName, "server-v") {
		return SelectedRelease{}, coded(CodeInvalidRelease, errors.New("release tag has the wrong namespace"))
	}
	versionText := strings.TrimPrefix(release.TagName, "server-v")
	version, err := buildinfo.ParseVersion(versionText)
	if err != nil || "server-v"+version.String() != release.TagName {
		return SelectedRelease{}, coded(CodeInvalidRelease, errors.New("release tag is not strict semantic version"))
	}
	names, err := AssetNames(versionText, goos, goarch)
	if err != nil {
		return SelectedRelease{}, err
	}
	var archive, checksum *Asset
	for i := range release.Assets {
		asset := release.Assets[i]
		if asset.Name == names.Archive {
			if archive != nil {
				return SelectedRelease{}, coded(CodeInvalidRelease, errors.New("release has duplicate platform archives"))
			}
			archive = &asset
		}
		if asset.Name == names.Checksum {
			if checksum != nil {
				return SelectedRelease{}, coded(CodeInvalidRelease, errors.New("release has duplicate checksum manifests"))
			}
			checksum = &asset
		}
	}
	if archive == nil || checksum == nil || archive.Size <= 0 || checksum.Size <= 0 {
		return SelectedRelease{}, coded(CodeInvalidRelease, errors.New("release assets are incomplete"))
	}
	return SelectedRelease{Version: version, TagName: release.TagName, Prerelease: release.Prerelease, Archive: *archive, Checksum: *checksum, PublishedAt: release.PublishedAt, PlatformName: goos + "/" + goarch}, nil
}

func SelectLatestForRuntime(releases []Release, channel Channel) (SelectedRelease, error) {
	return SelectLatest(releases, channel, runtime.GOOS, runtime.GOARCH)
}
