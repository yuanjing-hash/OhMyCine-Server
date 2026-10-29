package services

import "testing"

func TestContentModeKeepsManualAllDistinctFromPrivateProof(t *testing.T) {
	for _, source := range []string{"url", "torrent"} {
		if err := validateSiteContentMode(SiteDownloadInput{ContentMode: "all"}, source); err != nil {
			t.Fatal(err)
		}
	}
	for _, input := range []SiteDownloadInput{{ContentMode: "files"}, {ContentMode: "invalid"}, {ContentMode: "all", ContentSelectionToken: "opaque"}, {ContentMode: "all", FollowSubscriptionID: "follow"}, {ContentMode: "all", ReplacementIdentity: &DownloadRecognitionIdentity{TMDBID: 1}}} {
		if validateSiteContentMode(input, "url") == nil {
			t.Fatalf("invalid content intent accepted: %+v", input)
		}
	}
	if validateSiteContentMode(SiteDownloadInput{ContentMode: "all"}, "115_share") == nil || validateSiteContentMode(SiteDownloadInput{ContentMode: "all"}, "quark_share") == nil {
		t.Fatal("share inspection failure became whole-resource permission")
	}
	if err := validateSiteContentMode(SiteDownloadInput{ContentMode: "files", ContentSelectionToken: "opaque"}, "torrent"); err != nil {
		t.Fatal(err)
	}
	if err := validateSiteContentMode(SiteDownloadInput{}, "115_share"); err != nil {
		t.Fatal("legacy input contract changed")
	}
}
