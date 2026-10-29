package services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
)

type payloadProjectionAdapter struct {
	*stubSiteAdapter
	payload sitepkg.PayloadKind
}

func (a *payloadProjectionAdapter) Search(context.Context, sitepkg.Config, sitepkg.Query) (sitepkg.Page, error) {
	return sitepkg.Page{Page: 1, Items: []sitepkg.Result{{PayloadKind: a.payload, TorrentID: "private-provider-identity", Title: "Fixture.Show.S01E01"}}}, nil
}

func TestSearchResultPayloadProjectionIsFiniteAndNeverExposesSource(t *testing.T) {
	sites, stub, actor, _, _, _ := siteFixture(t)
	adapter := &payloadProjectionAdapter{stubSiteAdapter: stub}
	sites.adapters[stub.Kind()] = adapter
	created, err := sites.Create(context.Background(), actor, validSiteInput("Payload projection", "https://fixture.example.test"), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []sitepkg.PayloadKind{sitepkg.PayloadTorrent, sitepkg.PayloadMagnet, sitepkg.PayloadShare, "untrusted-private-uri"} {
		adapter.payload = input
		groups, err := sites.Search(context.Background(), actor, SiteSearchInput{Keyword: "Fixture", SiteID: &created.ID, Page: 1})
		if err != nil || len(groups) != 1 || len(groups[0].Items) != 1 {
			t.Fatalf("fixture search failed: %+v, %v", groups, err)
		}
		result := groups[0].Items[0]
		want := sitepkg.NormalizePayloadKind(input)
		if result.PayloadKind != want {
			t.Fatalf("safe projection=%q want=%q", result.PayloadKind, want)
		}
		if result.ContentInspection != (want != sitepkg.PayloadUnknown) {
			t.Fatal("legacy unknown adapter advertised a modern inspection contract")
		}
		claim, err := sites.resolveAvailableClaim(result.Token, actor.User.ID)
		if err != nil || claim.PayloadKind != want {
			t.Fatalf("private claim lost canonical payload: %q, %v", claim.PayloadKind, err)
		}
		encoded, err := json.Marshal(result)
		if err != nil || strings.Contains(string(encoded), "private-provider-identity") || strings.Contains(string(encoded), "untrusted-private-uri") {
			t.Fatal("payload projection exposed non-contract source data")
		}
	}
	if stub.downloads != 0 {
		t.Fatal("display metadata resolved or downloaded the source")
	}
}
