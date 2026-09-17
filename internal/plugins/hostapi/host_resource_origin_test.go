package hostapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
)

func TestResourceCredentialOriginNormalization(t *testing.T) {
	for _, tc := range []struct {
		entry, target string
		allowed       bool
	}{
		{"https://www.xn--kivn76b41nnhi.com/", "https://www.星际穿越.com/search", true},
		{"https://www.星际穿越.com", "https://WWW.XN--KIVN76B41NNHI.COM/search", true},
		{"https://mirror.example/", "https://mirror.example/search?q=test", true},
		{"https://mirror.example", "https://other.example/search", false},
		{"https://mirror.example", "http://mirror.example/search", false},
		{"https://mirror.example", "https://mirror.example:444/search", false},
		{"https://mirror.example", "https://mirror.example.evil.test/search", false},
		{"", "https://mirror.example/search", false},
	} {
		t.Run(tc.entry+" -> "+tc.target, func(t *testing.T) {
			target, err := url.Parse(tc.target)
			if err != nil {
				t.Fatal(err)
			}
			if got := resourceCredentialOriginAllowed(models.PluginConnection{ResourceType: "bt_resource", EntryOrigin: tc.entry}, target); got != tc.allowed {
				t.Fatalf("allowed=%v want=%v", got, tc.allowed)
			}
		})
	}
}

func TestHostHTTPResourceCredentialCannotCrossPermittedMirrors(t *testing.T) {
	fixture := newHostFixture(t, []contract.Permission{
		{Kind: contract.PermissionNetworkHTTP, Domains: []string{"api.example.test", "other.example.test"}},
		{Kind: contract.PermissionCredentialUse, Scopes: []string{"site.session"}},
	})
	if err := fixture.db.Model(&models.PluginConnection{}).Where("id = ?", fixture.connection.ID).Updates(map[string]any{
		"resource_type": "bt_resource", "entry_origin": "https://api.example.test/",
	}).Error; err != nil {
		t.Fatal(err)
	}
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.URL.Hostname() != "api.example.test" {
			t.Fatal("cross-mirror request reached transport")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Set-Cookie": {"session=test; Path=/; Secure"}}, Body: io.NopCloser(strings.NewReader("ok")), Request: request}, nil
	})}
	host := New(fixture.db, fixture.credentials, zerolog.Nop(), WithHTTPClient(client), WithResolver(publicResolver))
	for _, capture := range []bool{false, true} {
		input := httpRequest{ConnectionID: fixture.connection.ID, Method: http.MethodGet, URL: "https://other.example.test/search"}
		if capture {
			input.CaptureCredentialScope = "site.session"
		} else {
			input.Credential = "site.session"
		}
		payload, _ := json.Marshal(input)
		if _, err := host.Call(context.Background(), fixture.pluginID, OperationHTTP, payload); ErrorCode(err) != "plugin_credential_origin_denied" {
			t.Fatalf("capture=%v err=%v", capture, err)
		}
		if requests != 0 {
			t.Fatal("denied request used network")
		}
	}
	input := httpRequest{ConnectionID: fixture.connection.ID, Method: http.MethodGet, URL: "https://api.example.test/login", CaptureCredentialScope: "site.session", Credential: "site.session"}
	payload, _ := json.Marshal(input)
	raw, err := host.Call(context.Background(), fixture.pluginID, OperationHTTP, payload)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Data httpResponse `json:"data"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.CredentialCaptureRef == "" || requests != 1 {
		t.Fatal("same-mirror capture failed")
	}
	if err := fixture.db.Model(&models.PluginConnection{}).Where("id = ?", fixture.connection.ID).Update("entry_origin", "https://other.example.test").Error; err != nil {
		t.Fatal(err)
	}
	commit, _ := json.Marshal(credentialCommitRequest{ConnectionID: fixture.connection.ID, Scope: "site.session", CaptureRef: response.Data.CredentialCaptureRef})
	if _, err := host.Call(context.Background(), fixture.pluginID, OperationCredentialCommit, commit); ErrorCode(err) != "plugin_credential_origin_denied" {
		t.Fatalf("capture survived mirror change: %v", err)
	}
}
