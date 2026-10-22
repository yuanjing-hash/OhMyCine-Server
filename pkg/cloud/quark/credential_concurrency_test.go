package quark

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

func TestCredentialUpdatesCannotDiscardConsumedRotation(t *testing.T) {
	for _, competing := range []string{"agent_login", "root_proof"} {
		t.Run(competing, func(t *testing.T) {
			stored := fixtureCredentials()
			var mu sync.Mutex
			config := cloud.Config{ConnectionID: 17122,
				LoadCredentials: func(context.Context) (json.RawMessage, error) {
					mu.Lock()
					defer mu.Unlock()
					return json.Marshal(stored)
				},
				PersistCredentials: func(_ context.Context, expected uint64, raw json.RawMessage) error {
					mu.Lock()
					defer mu.Unlock()
					if expected != stored.Revision {
						return failure(CodeIdentity)
					}
					next, err := ParseCredentials(raw)
					if err == nil {
						stored = next
					}
					return err
				},
			}
			config.Credentials, _ = json.Marshal(stored)
			d, err := NewClient(config)
			if err != nil {
				t.Fatal(err)
			}
			a := NewAuthenticator(config)
			brokerEntered, releaseBroker := make(chan struct{}), make(chan struct{})
			transport := transportFunc(func(r *http.Request) (*http.Response, error) {
				switch {
				case r.URL.Host == "api.extscreen.com":
					close(brokerEntered)
					<-releaseBroker
					return response(r, 200, map[string]any{"code": 200, "data": map[string]any{"status": 0, "access_token": "rotated-tv-access", "refresh_token": "rotated-tv-refresh"}}), nil
				case r.URL.Path == "/user":
					return envelope(r, map[string]any{"user_id": "tv-account"}), nil
				case r.URL.Path == "/agent/v1/oauth/get_aac_by_pagecode":
					return envelope(r, map[string]any{"status": "success", "agent_auth_code": "new-agent-code"}), nil
				case r.URL.Path == "/agent/v1/oauth/agent_auth_code":
					return envelope(r, map[string]any{"access_token": "new-agent-access", "refresh_token": "new-agent-refresh", "device_id": "agent-device", "user_id": "agent-account"}), nil
				case r.URL.Path == "/open/v1/user/info":
					return envelope(r, map[string]any{"user_id": "agent-account"}), nil
				default:
					t.Errorf("unexpected route: %s", r.URL.Path)
					return nil, failure(CodeInvalid)
				}
			})
			d.api.http.Transport, a.api.http.Transport = transport, transport
			rotationDone := make(chan error, 1)
			go func() { rotationDone <- d.refresh(context.Background(), RouteTV, 1) }()
			<-brokerEntered
			otherDone := make(chan error, 1)
			otherStarted := make(chan struct{})
			go func() {
				close(otherStarted)
				if competing == "agent_login" {
					ch := Challenge{Route: RouteAgent, Revision: 1, DeviceID: "agent-device", ClientDeviceID: "client-device", PageCode: "page-code", ExpiresAt: time.Now().Add(time.Minute)}
					_, complete, e := a.Poll(context.Background(), ch, fixtureCredentials(), config.PersistCredentials)
					if e == nil && !complete {
						e = failure(CodeAuth)
					}
					otherDone <- e
					return
				}
				otherDone <- d.persistProof(context.Background(), SpaceProof{Version: 1, Generation: 1, TVAccount: "tv-account", AgentAccount: "agent-account", TVRoot: "tv-root", AgentRoot: "agent-root", ReceiptID: "proof", ContentSHA256: "proof-sha", VerifiedAt: time.Now()})
			}()
			<-otherStarted
			// A token response has not returned yet. No other bundle writer may
			// pass it and force the replacement checkpoint to lose its CAS.
			var early *error
			select {
			case e := <-otherDone:
				early = &e
			case <-time.After(50 * time.Millisecond):
			}
			close(releaseBroker)
			rotationErr := <-rotationDone
			var otherErr error
			if early != nil {
				otherErr = *early
			} else {
				otherErr = <-otherDone
			}
			if rotationErr != nil || otherErr != nil || stored.TV.RefreshToken != "rotated-tv-refresh" || !stored.TV.IdentityVerified {
				t.Fatalf("consumed rotation lost: rotation=%v competing=%v saved_rotation=%t", rotationErr, otherErr, stored.TV.RefreshToken == "rotated-tv-refresh")
			}
			if early != nil {
				t.Fatal("another credential writer overtook an in-flight rotation")
			}
			if competing == "agent_login" && stored.Agent.RefreshToken != "new-agent-refresh" || competing == "root_proof" && len(stored.Proofs) != 1 {
				t.Fatal("competing update did not preserve the other route or proof")
			}
		})
	}
}

func TestAuthenticatorReloadRejectsCancelledOrChangedConfigurationBeforeExchange(t *testing.T) {
	for _, mode := range []string{"cancelled", "configuration_changed"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			a := NewAuthenticator(cloud.Config{ConnectionID: 17123, LoadCredentials: func(context.Context) (json.RawMessage, error) {
				return nil, failure(CodeUnavailable)
			}})
			calls := 0
			a.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				return nil, failure(CodeInvalid)
			})
			persist := func(context.Context, uint64, json.RawMessage) error {
				t.Fatal("rejected authorization wrote credentials")
				return nil
			}
			old := fixtureCredentials()
			ch := Challenge{Route: RouteAgent, Revision: old.Revision, ExpiresAt: time.Now().Add(time.Minute)}
			if _, _, err := a.Poll(ctx, ch, old, persist); err == nil {
				t.Fatal("rejected poll succeeded")
			}
			if _, err := a.Refresh(ctx, RouteTV, old, persist); err == nil {
				t.Fatal("rejected refresh succeeded")
			}
			if _, err := a.Confirm(ctx, RouteTV, old, persist); err == nil {
				t.Fatal("rejected confirmation succeeded")
			}
			if calls != 0 {
				t.Fatal("authorization exchanged credentials after configuration/cancellation fence")
			}
		})
	}
}

func TestAuthenticatorRefreshReusesSavedRotationFromAnotherCaller(t *testing.T) {
	previous := fixtureCredentials()
	current := cloneCredentials(previous)
	current.TV.Generation++
	current.TV.AccessToken, current.TV.RefreshToken = "saved-access", "saved-refresh"
	current.Revision += 2
	a := NewAuthenticator(cloud.Config{ConnectionID: 17124, LoadCredentials: func(context.Context) (json.RawMessage, error) {
		return json.Marshal(current)
	}})
	a.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatal("stale caller unnecessarily rotated the already replaced token")
		return nil, failure(CodeInvalid)
	})
	result, err := a.Refresh(context.Background(), RouteTV, previous, func(context.Context, uint64, json.RawMessage) error {
		t.Fatal("stale caller overwrote the saved rotation")
		return nil
	})
	if err != nil || result.TV.RefreshToken != current.TV.RefreshToken || result.Revision != current.Revision {
		t.Fatalf("saved rotation not reused: %v", err)
	}
}
