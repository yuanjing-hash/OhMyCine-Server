package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
)

type contentRouterAdapter struct{ routerSiteAdapter }

func (contentRouterAdapter) Download(context.Context, sitepkg.Config, string) ([]byte, string, error) {
	name := "Actual.Show.S01E13.mkv"
	raw := []byte(fmt.Sprintf("d4:infod6:lengthi100e4:name%d:%s12:piece lengthi16384e6:pieces20:", len(name), name))
	raw = append(raw, bytes.Repeat([]byte{1}, 20)...)
	raw = append(raw, 'e', 'e')
	return raw, "fixture.torrent", nil
}

func TestResourceContentSessionAndPlayerRoutesUseOpaqueActualFiles(t *testing.T) {
	client := newTestClientWithSiteAdapters(t, []sitepkg.Adapter{contentRouterAdapter{}})
	for _, endpoint := range []string{"content-preview", "content-selection"} {
		status, _ := client.request(t, http.MethodPost, "/api/v1/discovery/"+endpoint, map[string]any{}, false)
		if status != http.StatusUnauthorized {
			t.Fatalf("anonymous %s status=%d", endpoint, status)
		}
	}
	client.setup(t)
	var owner models.User
	if err := client.db.Where("username_normalized = ?", "owner").First(&owner).Error; err != nil {
		t.Fatal(err)
	}
	actor, err := services.NewAuthorizationService(client.db).Resolve(owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	site, err := client.sites.Create(context.Background(), actor, services.SiteInput{Name: "Content HTTP fixture", Kind: "pttime", BaseURL: "https://pt.example.test", Cookie: "fixture-cookie", Enabled: true, Priority: 1, TimeoutSeconds: 15, RateLimitPerMinute: 120}, services.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	groups, err := client.sites.Search(context.Background(), actor, services.SiteSearchInput{Keyword: "Seven", SiteID: &site.ID, Page: 1})
	if err != nil || len(groups) != 1 || len(groups[0].Items) != 1 {
		t.Fatalf("fixture results=%+v err=%v", groups, err)
	}
	token := groups[0].Items[0].Token
	status, envelope := client.request(t, http.MethodPost, "/api/v1/discovery/content-preview", map[string]any{"result_token": token}, false)
	if status != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d", status)
	}
	status, envelope = client.request(t, http.MethodPost, "/api/v1/discovery/content-preview", map[string]any{"result_token": token}, true)
	var preview services.ContentPreviewSummary
	if status != http.StatusOK || json.Unmarshal(envelope.Data, &preview) != nil || !preview.Complete || len(preview.Files) != 1 || preview.Files[0].Name != "Actual.Show.S01E13.mkv" {
		t.Fatalf("actual preview status=%d message=%s data=%s", status, envelope.Message, envelope.Data)
	}
	if client.lastHeader.Get("Cache-Control") != "no-store" {
		t.Fatal("inspection response was cacheable")
	}
	for _, secret := range []string{"fixture-cookie", "pt.example.test", "\"index\"", "\"v1\"", "announce"} {
		if strings.Contains(string(envelope.Data), secret) {
			t.Fatalf("private source field %q exposed", secret)
		}
	}
	status, envelope = client.request(t, http.MethodPost, "/api/v1/discovery/content-selection", map[string]any{"result_token": token, "preview_token": preview.Token, "selected_entry_tokens": []string{preview.Files[0].Token}}, true)
	var selection services.ContentSelectionSummary
	if status != http.StatusOK || json.Unmarshal(envelope.Data, &selection) != nil || selection.Token == "" || selection.SelectedBytes != 100 {
		t.Fatalf("opaque selection status=%d data=%s", status, envelope.Data)
	}
	status, _ = client.request(t, http.MethodPost, "/api/v1/discovery/content-selection", map[string]any{"result_token": token, "preview_token": preview.Token, "selected_entry_tokens": []string{preview.Files[0].Token}, "indices": []int{0}}, true)
	if status != http.StatusBadRequest {
		t.Fatalf("browser native index accepted %d", status)
	}
	status, loginEnvelope, _ := client.playerRequest(t, http.MethodPost, "/api/v1/player/auth/login", "", map[string]any{"username": "owner", "password": "strong-owner-password", "device_id": "content-inspection-test", "device_name": "Content fixture"})
	var login struct {
		AccessToken string `json:"access_token"`
	}
	if status != http.StatusOK || json.Unmarshal(loginEnvelope.Data, &login) != nil {
		t.Fatalf("Player fixture login %d", status)
	}
	status, envelope, headers := client.playerRequest(t, http.MethodPost, "/api/v1/player/discovery/content-preview", login.AccessToken, map[string]any{"result_token": token})
	if status != http.StatusOK || headers.Get("Cache-Control") != "no-store" || json.Unmarshal(envelope.Data, &preview) != nil {
		t.Fatalf("Player preview status=%d message=%s", status, envelope.Message)
	}
	status, _, _ = client.playerRequest(t, http.MethodPost, "/api/v1/player/discovery/content-selection", login.AccessToken, map[string]any{"result_token": token, "preview_token": preview.Token, "selected_entry_tokens": []string{preview.Files[0].Token}})
	if status != http.StatusOK {
		t.Fatalf("Player selection status=%d", status)
	}
	var count int64
	client.db.Model(&models.DownloadTask{}).Count(&count)
	if count != 0 {
		t.Fatal("read/selection API created an external download task")
	}
}
