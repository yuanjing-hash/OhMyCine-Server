package updater

import (
	"fmt"
	"testing"
)

func fixtureRelease(version string, prerelease, draft bool) Release {
	names, _ := AssetNames(version, "windows", "amd64")
	base := "https://github.com/yuanjing-hash/OhMyCine-Server/releases/download/server-v" + version + "/"
	return Release{TagName: "server-v" + version, Prerelease: prerelease, Draft: draft, Assets: []Asset{
		{Name: names.Archive, DownloadURL: base + names.Archive, Size: 1024},
		{Name: names.Checksum, DownloadURL: base + names.Checksum, Size: 128},
	}}
}

func TestSelectLatestHonorsChannels(t *testing.T) {
	releases := []Release{fixtureRelease("1.2.0", false, false), fixtureRelease("1.3.0", true, false), fixtureRelease("9.0.0", false, true)}
	beta, err := SelectLatest(releases, ChannelBeta, "windows", "amd64")
	if err != nil || beta.Version.String() != "1.3.0" {
		t.Fatalf("unexpected beta: %+v err=%v", beta, err)
	}
	stable, err := SelectLatest(releases, ChannelStable, "windows", "amd64")
	if err != nil || stable.Version.String() != "1.2.0" {
		t.Fatalf("unexpected stable: %+v err=%v", stable, err)
	}
}

func TestReleaseValidationRejectsMalformedIdentityAndAssets(t *testing.T) {
	base := fixtureRelease("1.2.3", false, false)
	tests := []struct {
		name string
		edit func(*Release)
	}{
		{"wrong namespace", func(r *Release) { r.TagName = "v1.2.3" }},
		{"leading zero", func(r *Release) { r.TagName = "server-v01.2.3" }},
		{"missing checksum", func(r *Release) { r.Assets = r.Assets[:1] }},
		{"duplicate archive", func(r *Release) { r.Assets = append(r.Assets, r.Assets[0]) }},
		{"zero size", func(r *Release) { r.Assets[0].Size = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			release := base
			release.Assets = append([]Asset(nil), base.Assets...)
			test.edit(&release)
			if _, err := ValidateRelease(release, "windows", "amd64"); ErrorCode(err) != CodeInvalidRelease {
				t.Fatalf("expected invalid release, got %v", err)
			}
		})
	}
	if _, err := AssetNames("1.2.3", "darwin", "amd64"); ErrorCode(err) != CodeUnsupportedPlatform {
		t.Fatalf("unexpected platform error: %v", err)
	}
	if _, err := SelectLatest(nil, Channel("nightly"), "windows", "amd64"); ErrorCode(err) != CodeInvalidChannel {
		t.Fatalf("unexpected channel error: %v", err)
	}
	_ = fmt.Sprintf("%v", base)
}
