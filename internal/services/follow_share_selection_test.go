package services

import (
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"reflect"
	"testing"
)

func TestFollowShareSelectionUsesRealMissingLeavesAndMatchingSidecars(t *testing.T) {
	entries := []SiteSharePreviewEntry{
		{Token: "dir", Path: "Show.S01.Complete", IsDir: true},
		{Token: "old", Path: "Show/Show.S01E01.mkv"},
		{Token: "new", Path: "Show/Show.S01E02.mkv"},
		{Token: "sub", Path: "Show/Show.S01E02.zh.srt"},
		{Token: "oldsub", Path: "Show/Show.S01E01.zh.srt"},
		{Token: "wrongseason", Path: "Show/Show.S02E02.mkv"},
		{Token: "unproven", Path: "Show/Bonus.mkv"},
	}
	tokens, episodes := selectFollowShareFiles(entries, 1, []int{2, 3})
	if !reflect.DeepEqual(tokens, []string{"new", "sub"}) || !reflect.DeepEqual(episodes, []int{2}) {
		t.Fatalf("selected %v episodes %v", tokens, episodes)
	}
	tokens, episodes = selectFollowShareFiles(entries, 1, []int{3})
	if len(tokens) != 0 || len(episodes) != 0 {
		t.Fatal("share title claimed absent episodes")
	}
}

func TestQuarkSourceRoutePinsProviderAndFirstReceiver(t *testing.T) {
	for _, source := range []string{RouteSource115Share, RouteSourceQuarkShare} {
		want := "pan115_offline"
		if source == RouteSourceQuarkShare {
			want = "quark_share"
		}
		for _, downloader := range []string{"qbittorrent", "pan115_offline", "quark_share"} {
			if routeDownloaderApplicable(source, models.Downloader{Type: downloader}) != (downloader == want) {
				t.Fatalf("source %s chose %s", source, downloader)
			}
		}
	}
}
