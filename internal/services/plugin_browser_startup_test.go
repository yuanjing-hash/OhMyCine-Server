package services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/browsercompanion"
)

type startupBrowserFixture struct {
	browserFixture
	code  string
	state string
}

func (b *startupBrowserFixture) Call(ctx context.Context, operation string, input, output any) error {
	if operation == "session/create" && b.code != "" {
		return &browsercompanion.Failure{Code: b.code}
	}
	if operation == "status" {
		b.calls = append(b.calls, operation)
		raw, _ := json.Marshal(map[string]any{"state": b.state, "installed": true, "runtimeError": b.code, "protocolVersion": 1})
		return json.Unmarshal(raw, output)
	}
	return b.browserFixture.Call(ctx, operation, input, output)
}

func TestPluginBrowserStartupFailureRemainsActionableAndStatusDoesNotLaunch(t *testing.T) {
	s, actor, _, _, connection, _ := resourcePluginServiceFixture(t, "org.ohmycine.browser-startup")
	actor.Permissions[authz.PermissionSettingsRead] = struct{}{}
	browser := &startupBrowserFixture{state: "launch_failed"}
	s.browser = browser
	for _, code := range []string{"browser_sandbox_unavailable", "browser_dependencies_missing", "browser_executable_unavailable", "browser_launch_timeout", "browser_profile_unavailable"} {
		browser.code = code
		_, err := s.ResourceBrowser(context.Background(), actor, connection.PluginID, connection.ID, "start", BrowserInput{})
		if ErrorCode(err) != "resource_"+code || s.browserSession != nil {
			t.Fatal("startup code lost or failure published a session")
		}
		if strings.Contains(err.Error(), "synthetic-cookie") {
			t.Fatal("raw startup error escaped")
		}
		before := len(browser.calls)
		output, err := s.BrowserComponent(context.Background(), actor, "status", BrowserInput{})
		if err != nil {
			t.Fatal(err)
		}
		result := output.(map[string]any)
		if result["state"] != "launch_failed" || result["installed"] != true || result["runtime_error"] != code || len(browser.calls) != before+1 || browser.calls[before] != "status" {
			t.Fatal("status lost failure or launched browser")
		}
	}
	browser.code, browser.state = "synthetic-cookie https://private.invalid/?token=synthetic /private/profile", "/private/profile"
	output, err := s.BrowserComponent(context.Background(), actor, "status", BrowserInput{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(output)
	if strings.Contains(string(raw), "private") || strings.Contains(string(raw), "synthetic-cookie") || output.(map[string]any)["state"] != "unavailable" {
		t.Fatal("unsafe component fields escaped")
	}
	_, err = s.ResourceBrowser(context.Background(), actor, connection.PluginID, connection.ID, "start", BrowserInput{})
	if ErrorCode(err) != "resource_browser_start_failed" {
		t.Fatal("unknown startup should remain generic")
	}
	browser.code = ""
	if _, err := s.ResourceBrowser(context.Background(), actor, connection.PluginID, connection.ID, "start", BrowserInput{}); err != nil || s.browserSession == nil {
		t.Fatal("explicit retry failed to recover")
	}
}
