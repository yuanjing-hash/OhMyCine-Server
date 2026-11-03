package pansou

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
)

func TestOrdinaryReceivedBatchIsNotWarningOrComplete(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"code":0,"data":{"total":0,"results":[]}}`)
	}))
	defer s.Close()
	page, err := NewForTest(s.Client()).Search(context.Background(), site.Config{BaseURL: s.URL, Cloud: &site.CloudConfig{Provider: "115", Channels: []string{"movies"}, SourceScope: "tg", TypePolicy: "manual", CloudTypes: []string{"115"}}}, site.Query{Keyword: "Show", Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	if page.Completion != "received" || page.Partial || len(page.Warnings) != 0 {
		t.Fatalf("ordinary response=%+v", page)
	}
}
