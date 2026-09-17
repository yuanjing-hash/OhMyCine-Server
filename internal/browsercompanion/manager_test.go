package browsercompanion

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompanionReloadTransportUsesAuthenticatedSessionOperation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic" || r.Method != "POST" || r.URL.Path != "/v1/session/reload" {
			t.Error("reload route/auth contract mismatch")
		}
		var input map[string]string
		if json.NewDecoder(r.Body).Decode(&input) != nil || len(input) != 2 || input["sessionId"] != "opaque" || input["identity"] != "bound" {
			t.Error("reload must only carry session identity")
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	manager := &Manager{address: server.URL, token: "synthetic"}
	if err := manager.call(context.Background(), "session/reload", map[string]string{"sessionId": "opaque", "identity": "bound"}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestBrowserEnvironmentDoesNotInheritServerSecrets(t *testing.T) {
	t.Setenv("OMC_TEST_SECRET", "synthetic")
	t.Setenv("NODE_OPTIONS", "--inspect")
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid")
	for _, entry := range browserEnvironment() {
		if strings.HasPrefix(entry, "OMC_TEST_SECRET=") || strings.HasPrefix(entry, "NODE_OPTIONS=") || strings.HasPrefix(entry, "HTTPS_PROXY=") {
			t.Fatal("privileged environment inherited")
		}
	}
}

func TestTUNEnvironmentRequiresExactDeploymentOptIn(t *testing.T) {
	for _, value := range []string{"", "false", "1", "TRUE", "true ", "true"} {
		t.Setenv("OMC_CLOAK_TUN_FAKE_IP", value)
		expected := "OMC_CLOAK_TUN_FAKE_IP=false"
		if value == "true" {
			expected = "OMC_CLOAK_TUN_FAKE_IP=true"
		}
		count := 0
		for _, entry := range browserEnvironment() {
			if strings.HasPrefix(entry, "OMC_CLOAK_TUN_FAKE_IP=") {
				count++
				if entry != expected {
					t.Fatalf("unsafe flag normalization: %q", entry)
				}
			}
		}
		if count != 1 {
			t.Fatal("missing or duplicated flag")
		}
	}
}

func TestCompanionPreservesSafeTUNFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"tun_fake_ip_requires_opt_in"}`))
	}))
	defer server.Close()
	manager := &Manager{address: server.URL, token: "synthetic"}
	if ErrorCode(manager.call(context.Background(), "session/create", nil, nil)) != "tun_fake_ip_requires_opt_in" {
		t.Fatal("TUN diagnosis lost at IPC boundary")
	}
}

func TestCompanionProtocolTransport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-test-only" || r.Method != "POST" || r.URL.Path != "/v1/status" {
			t.Error("request contract mismatch")
		}
		_, _ = w.Write([]byte(`{"state":"not_installed","protocolVersion":1}`))
	}))
	defer server.Close()
	manager := &Manager{address: server.URL, token: "synthetic-test-only"}
	var response struct {
		State string `json:"state"`
	}
	if err := manager.call(context.Background(), "status", struct{}{}, &response); err != nil || response.State != "not_installed" {
		t.Fatalf("err=%v state=%s", err, response.State)
	}
	if manager.call(context.Background(), "arbitrary", nil, nil) == nil {
		t.Fatal("unapproved operation")
	}
}

func TestCompanionDoesNotFollowRedirect(t *testing.T) {
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("credential forwarded") }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	manager := &Manager{address: server.URL, token: "synthetic"}
	if manager.call(context.Background(), "status", nil, nil) == nil {
		t.Fatal("redirect accepted")
	}
}
