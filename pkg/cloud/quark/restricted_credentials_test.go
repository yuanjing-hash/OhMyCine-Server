package quark

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func accessOnlyFixture(t *testing.T) *Client {
	t.Helper()
	d := fixtureClient(t, func(*http.Request) (*http.Response, error) { t.Fatal("unexpected API request"); return nil, nil })
	d.credentials.TV.RefreshToken = ""
	d.credentials.Agent.RefreshToken = ""
	return d
}

func TestRestrictedCredentialsLoadBeforeRequestAndKeepIdentity(t *testing.T) {
	d := accessOnlyFixture(t)
	loads := 0
	next := d.Credentials()
	next.TV.AccessToken = "rotated-tv"
	next.Agent.AccessToken = "rotated-agent"
	next.Revision++
	next.TV.Generation++
	if e := d.SetRestrictedCredentialLoader(func(context.Context) (json.RawMessage, error) { loads++; return json.Marshal(next) }); e != nil {
		t.Fatal(e)
	}
	d.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if loads == 0 || r.URL.Query().Get("access_token") != "rotated-tv" {
			t.Fatal("request captured old grant")
		}
		return envelope(r, map[string]any{"fid": "f", "file_name": "f.mkv", "size": 4, "download_url": "https://cdn.example/f"}), nil
	})
	if _, _, e := d.original(context.Background(), "f"); e != nil {
		t.Fatal(e)
	}
	if got, e := d.uploadAuthorization(context.Background(), *fixtureCredentials().Agent); e != nil || got.AccessToken != "rotated-agent" {
		t.Fatal("finish did not reload grant", e)
	}
}

func TestRestrictedCredentialsRejectScopeChangeAndLoaderFailure(t *testing.T) {
	for _, mode := range []string{"account", "device", "client_device", "proof", "generation", "revision", "same_revision_token", "refresh", "web", "route", "unverified", "error"} {
		t.Run(mode, func(t *testing.T) {
			d := accessOnlyFixture(t)
			before := d.Credentials()
			next := cloneCredentials(before)
			switch mode {
			case "account":
				next.Agent.AccountID = "other"
			case "device":
				next.Agent.DeviceID = "other"
			case "client_device":
				next.Agent.ClientDeviceID = "other"
			case "proof":
				next.Proofs = []SpaceProof{{TVRoot: "other"}}
			case "generation":
				next.Agent.Generation = 0
			case "revision":
				next.Revision = 0
			case "same_revision_token":
				next.Agent.AccessToken = "other"
			case "refresh":
				next.Agent.RefreshToken = "secret"
			case "web":
				next.Web = &WebAuthorization{Cookie: "secret"}
			case "route":
				next.Agent = nil
			case "unverified":
				next.Agent.IdentityVerified = false
			}
			if e := d.SetRestrictedCredentialLoader(func(context.Context) (json.RawMessage, error) {
				if mode == "error" {
					return nil, errors.New("grant denied")
				}
				return json.Marshal(next)
			}); e != nil {
				t.Fatal(e)
			}
			if _, e := d.authorization(context.Background(), RouteAgent); e == nil {
				t.Fatal("changed scope accepted")
			}
			b, _ := json.Marshal(before)
			a, _ := json.Marshal(d.Credentials())
			if string(a) != string(b) {
				t.Fatal("failed grant replaced credentials")
			}
		})
	}
	regular := fixtureClient(t, func(*http.Request) (*http.Response, error) { return nil, nil })
	if e := regular.SetRestrictedCredentialLoader(func(context.Context) (json.RawMessage, error) { return nil, nil }); e == nil {
		t.Fatal("full credential bundle accepted")
	}
}
