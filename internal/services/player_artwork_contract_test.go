package services

import (
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
)

func TestPlayerCatalogArtworkContractUsesDeviceRoute(t *testing.T) {
	client, err := tmdb.NewWithCredential(tmdb.Credential{Kind: tmdb.CredentialKindReadAccessToken, Value: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	poster := playerCatalogImageURLWithClient(client, "/poster.jpg", "w500")
	if !strings.HasPrefix(poster, "/api/v1/player/discovery/images/tmdb/") || browserArtworkURL(poster) != catalogImageURLWithClient(client, "/poster.jpg", "w500") {
		t.Fatalf("Player and browser artwork routes diverged: %q", poster)
	}
	if playerCatalogImageURLWithClient(client, "https://unsafe.example/image.jpg", "w500") != "" {
		t.Fatal("untrusted absolute TMDB image identity accepted")
	}
	people := playerMediaPeople(tmdb.Snapshot{Cast: []tmdb.Person{{Name: "演员", ProfilePath: "/person.jpg"}, {Name: "无效", ProfilePath: "https://unsafe.example/person.jpg"}}}, client)
	if len(people) != 2 || !strings.HasPrefix(people[0].ProfileURL, "/api/v1/player/discovery/images/tmdb/") || people[1].ProfileURL != "" {
		t.Fatalf("person image projection=%+v", people)
	}
	stills := playerStillURLs(client, []string{"/still.jpg", "https://unsafe.example/still.jpg"})
	if len(stills) != 1 || !strings.HasPrefix(stills[0], "/api/v1/player/discovery/images/tmdb/") {
		t.Fatalf("still image projection=%+v", stills)
	}
	serverHistory := playerHistoryArtworkDTO(PlayerHistoryChange{SourceKind: "server", PosterPath: "/poster.jpg", BackdropPath: "/backdrop.jpg", EpisodeStillPath: "/episode.jpg", PosterURL: "https://attacker.example/forged.jpg"}, client)
	for _, imageURL := range []string{serverHistory.PosterURL, serverHistory.BackdropURL, serverHistory.EpisodeStillURL} {
		if !strings.HasPrefix(imageURL, "/api/v1/player/discovery/images/tmdb/") {
			t.Fatalf("Server history exposed external artwork: %+v", serverHistory)
		}
	}
	embyHistory := playerHistoryArtworkDTO(PlayerHistoryChange{SourceKind: "emby", PosterURL: "https://emby.example/image.jpg"}, client)
	if embyHistory.PosterURL != "https://emby.example/image.jpg" {
		t.Fatalf("direct Emby artwork unexpectedly changed: %+v", embyHistory)
	}
}
