package hostapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestHostBrowserFakeIPDNSAndPrivateBoundary(t *testing.T) {
	fixture := newHostFixture(t, []contract.Permission{{Kind: contract.PermissionNetworkHTTP, Domains: []string{"api.example.test"}}, {Kind: contract.PermissionCredentialUse, Scopes: []string{"site.session"}}})
	for _, address := range []string{"198.18.7.137", "192.168.1.1", "127.0.0.1", "169.254.169.254", "fc00::1"} {
		host := New(fixture.db, fixture.credentials, zerolog.Nop(), WithResolver(func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP(address)}}, nil
		}))
		called := false
		host.SetBrowserRequest(func(_ context.Context, _, _, _ string, r *http.Request) (*http.Response, bool, error) {
			called = true
			if r.URL.Host != "api.example.test" {
				t.Fatal("original hostname lost")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, true, nil
		})
		raw, _ := json.Marshal(httpRequest{ConnectionID: fixture.connection.ID, Credential: "site.session", Method: "GET", URL: "https://api.example.test/search"})
		_, err := host.Call(context.Background(), fixture.pluginID, OperationHTTP, raw)
		if address == "198.18.7.137" {
			if err != nil || !called {
				t.Fatalf("Fake-IP cannot reach managed browser policy: %v", err)
			}
		} else if err == nil || called {
			t.Fatal("private destination reached browser")
		}
	}
}

func TestBrowserLoginCapturePreservesBinaryAndCommitsEncryptedCookie(t *testing.T) {
	fixture := newHostFixture(t, []contract.Permission{{Kind: contract.PermissionNetworkHTTP, Domains: []string{"api.example.test"}}, {Kind: contract.PermissionCredentialUse, Scopes: []string{"site.session"}}})
	if err := fixture.db.Model(&models.PluginConnection{}).Where("id = ?", fixture.connection.ID).Updates(map[string]any{"credential_ciphertext": "", "credential_scope": "site.session", "credential_mode": models.PluginCredentialModeCookie}).Error; err != nil {
		t.Fatal(err)
	}
	host := New(fixture.db, fixture.credentials, zerolog.Nop(), WithResolver(publicResolver))
	binary := []byte{137, 80, 78, 71, 255, 0, 192, 128}
	host.SetBrowserRequest(func(_ context.Context, _, _, scope string, r *http.Request) (*http.Response, bool, error) {
		if scope != "site.session" || r.Header.Get("Cookie") != "" {
			t.Fatal("invalid browser capture scope")
		}
		return &http.Response{StatusCode: 200, Request: r, Header: http.Header{"Content-Type": {"image/png"}, "Set-Cookie": {"session=synthetic-only; Path=/; Secure; HttpOnly"}}, Body: io.NopCloser(bytes.NewReader(binary))}, true, nil
	})
	committed := false
	host.SetBrowserCommit(func(ctx context.Context, before models.PluginConnection) {
		committed = browserAuthenticationAllowed(ctx, fixture.pluginID, before.ID)
	})
	ctx := WithBrowserAuthentication(context.Background(), fixture.pluginID, fixture.connection.ID)
	payload, _ := json.Marshal(httpRequest{ConnectionID: fixture.connection.ID, Method: "GET", URL: "https://api.example.test/captcha", CaptureCredentialScope: "site.session"})
	raw, err := host.Call(ctx, fixture.pluginID, OperationHTTP, payload)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("synthetic-only")) {
		t.Fatal("cookie leaked to plugin")
	}
	var envelope struct {
		Data httpResponse `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(envelope.Data.BodyBase64)
	if err != nil || !bytes.Equal(decoded, binary) {
		t.Fatal("binary captcha corrupted")
	}
	var before models.PluginConnection
	fixture.db.First(&before, "id = ?", fixture.connection.ID)
	if before.CredentialCiphertext != "" {
		t.Fatal("cookie persisted before successful commit")
	}
	payload, _ = json.Marshal(credentialCommitRequest{ConnectionID: fixture.connection.ID, Scope: "site.session", CaptureRef: envelope.Data.CredentialCaptureRef})
	if _, err := host.Call(ctx, fixture.pluginID, OperationCredentialCommit, payload); err != nil {
		t.Fatal(err)
	}
	if !committed {
		t.Fatal("browser context lost during credential commit")
	}
	var after models.PluginConnection
	fixture.db.First(&after, "id = ?", fixture.connection.ID)
	plain, err := fixture.credentials.Decrypt(CredentialPurpose(fixture.pluginID, fixture.connection.ID, "site.session"), after.CredentialCiphertext)
	if err != nil || plain != "session=synthetic-only" {
		t.Fatal("encrypted credential did not round-trip")
	}
}

func TestHostBrowserScopedAuthCaptureAndBinary(t *testing.T) {
	fixture := newHostFixture(t, []contract.Permission{{Kind: contract.PermissionNetworkHTTP, Domains: []string{"api.example.test"}}, {Kind: contract.PermissionCredentialUse, Scopes: []string{"site.session"}}})
	if err := fixture.db.Model(&models.PluginConnection{}).Where("id = ?", fixture.connection.ID).Updates(map[string]any{"resource_type": "bt_resource", "entry_origin": "https://api.example.test/", "credential_mode": "cookie"}).Error; err != nil {
		t.Fatal(err)
	}
	host := New(fixture.db, fixture.credentials, zerolog.Nop(), WithResolver(publicResolver))
	png := []byte{137, 80, 78, 71, 13, 10, 26, 10, 0, 255, 254}
	host.SetBrowserRequest(func(ctx context.Context, p, c, scope string, r *http.Request) (*http.Response, bool, error) {
		if !browserAuthenticationAllowed(ctx, p, c) {
			t.Fatal("auth operation context lost")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/png"}, "Set-Cookie": {"session=synthetic; Path=/; Secure; HttpOnly"}}, Request: r, Body: io.NopCloser(bytes.NewReader(png))}, true, nil
	})
	ctx := WithBrowserAuthentication(context.Background(), fixture.pluginID, fixture.connection.ID)
	input := httpRequest{ConnectionID: fixture.connection.ID, CaptureCredentialScope: "site.session", Method: "POST", URL: "https://api.example.test/login"}
	raw, _ := json.Marshal(input)
	output, err := host.Call(ctx, fixture.pluginID, OperationHTTP, raw)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Data httpResponse `json:"data"`
	}
	if json.Unmarshal(output, &envelope) != nil {
		t.Fatal("invalid response")
	}
	response := envelope.Data
	if response.CredentialCaptureRef == "" || bytes.Contains(output, []byte("synthetic")) {
		t.Fatal("capture not opaque")
	}
	body, err := base64.StdEncoding.DecodeString(response.BodyBase64)
	if err != nil || !bytes.Equal(body, png) {
		t.Fatal("binary challenge was corrupted")
	}
	commit, _ := json.Marshal(map[string]any{"connectionId": fixture.connection.ID, "scope": "site.session", "captureRef": response.CredentialCaptureRef})
	if _, err := host.Call(ctx, fixture.pluginID, OperationCredentialCommit, commit); err != nil {
		t.Fatal(err)
	}
	var saved models.PluginConnection
	fixture.db.First(&saved, "id = ?", fixture.connection.ID)
	plain, err := fixture.credentials.Decrypt(CredentialPurpose(fixture.pluginID, saved.ID, "site.session"), saved.CredentialCiphertext)
	if err != nil || !strings.Contains(plain, "session=synthetic") {
		t.Fatalf("capture not committed: %v", err)
	}
}

func TestHostBrowserPathRetainsNetworkPermissionAndNoCookieExport(t *testing.T) {
	fixture := newHostFixture(t, []contract.Permission{{Kind: contract.PermissionNetworkHTTP, Domains: []string{"api.example.test"}}, {Kind: contract.PermissionCredentialUse, Scopes: []string{"site.session"}}})
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Cookie") != "" {
			t.Error("public request acquired credential")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("public"))}, nil
	})}
	host := New(fixture.db, fixture.credentials, zerolog.Nop(), WithResolver(publicResolver), WithHTTPClient(client))
	calls := 0
	host.SetBrowserRequest(func(_ context.Context, p, c, scope string, r *http.Request) (*http.Response, bool, error) {
		calls++
		if r.Header.Get("Cookie") != "" || p != fixture.pluginID || c != fixture.connection.ID || scope != "site.session" {
			t.Error("browser credential boundary mismatch")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, true, nil
	})
	input := httpRequest{ConnectionID: fixture.connection.ID, Credential: "site.session", Method: "GET", URL: "https://api.example.test/search"}
	raw, _ := json.Marshal(input)
	if _, err := host.Call(context.Background(), fixture.pluginID, OperationHTTP, raw); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("browser was not used")
	}
	input.URL = "https://forbidden.example/search"
	raw, _ = json.Marshal(input)
	if _, err := host.Call(context.Background(), fixture.pluginID, OperationHTTP, raw); err == nil {
		t.Fatal("forbidden host accepted")
	}
	if calls != 1 {
		t.Fatal("denied request reached browser")
	}
	input.URL = "https://api.example.test/search"
	input.Credential = ""
	raw, _ = json.Marshal(input)
	if _, err := host.Call(context.Background(), fixture.pluginID, OperationHTTP, raw); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("uncredentialed request reached browser")
	}
}
