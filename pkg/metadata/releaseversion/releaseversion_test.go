package releaseversion

import "testing"

func TestParseExtractsOrderedReleaseVersionWithoutNoise(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"Seven.Samurai.1954.CC.2160p.UHD.BluRay.x265.10bit.DTS-HD.MA.2.0-SONYHD.mkv", "2160p UHD BluRay H265 10bit DTS-HD MA 2.0"},
		{"Blade.Runner.1982.Final.Cut.2160p.UHD.BluRay.REMUX.DV.HDR10.TrueHD.Atmos-GROUP.mkv", "Final Cut 2160p UHD BluRay REMUX HDR10 Dolby Vision TrueHD Atmos"},
		{"Aliens.1986.Extended.Directors.Cut.1080p.BluRay.REMUX-GROUP.mkv", "Extended Director's Cut 1080p BluRay REMUX"},
		{"Movie.2024.1080p.WEB-DL.HDR10Plus.DoVi.DDP5.1.H.265-GROUP.mkv", "1080p WEB-DL HDR10+ Dolby Vision H265 DDP 5.1"},
		{"电影.2024.导演剪辑版.2160p.WEB-DL.HDR10.mkv", "Director's Cut 2160p WEB-DL HDR10"},
		{"Ordinary.Movie.2024.x265.DTS-GROUP.mkv", "H265 DTS"},
		{"三傻大闹宝莱坞 (2009) 1080p h264 DTSHD-MA.mkv", "1080p H264 DTS-HD MA"},
		{"三傻大闹宝莱坞 (2009) 1080p h265 AC3.mkv", "1080p H265 AC3"},
		{"A.Movie.About.IMAX.2024.mkv", "IMAX"},
		{"DVD.Collection.2024.DVDRip.mkv", "DVDRip"},
	}
	for _, test := range tests {
		if got := Parse(test.name); got != test.want {
			t.Errorf("Parse(%q) = %q, want %q", test.name, got, test.want)
		}
	}
}

func TestCanonicalReleaseLabelsAreStable(t *testing.T) {
	for _, source := range []string{"Movie.1080p.x265.DTS-HD.MA.5.1.mkv", "Movie.2160p.DV.HDR10+.TrueHD.Atmos.mkv", "Movie.WEB-DL.H264.DDP5.1.mkv"} {
		label := Parse(source)
		if got := Parse("Movie - " + label + ".mkv"); got != label {
			t.Errorf("%q became %q", label, got)
		}
	}
}
