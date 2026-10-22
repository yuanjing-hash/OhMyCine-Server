package quark

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

func TestSavedRefreshIdentityResumesOnActualDirectoryRead(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "same_identity", true: "changed_identity"}[changed], func(t *testing.T) {
			stored := fixtureCredentials()
			stored.TV.IdentityVerified = false
			stored.TV.Generation++
			var mu sync.Mutex
			raw, _ := json.Marshal(stored)
			d, err := NewClient(cloud.Config{ConnectionID: 981,
				Credentials: raw,
				LoadCredentials: func(context.Context) (json.RawMessage, error) {
					mu.Lock()
					defer mu.Unlock()
					return json.Marshal(stored)
				},
				PersistCredentials: func(_ context.Context, expected uint64, raw json.RawMessage) error {
					mu.Lock()
					defer mu.Unlock()
					if expected != stored.Revision {
						t.Fatal("identity resume did not use latest saved revision")
					}
					next, parseErr := ParseCredentials(raw)
					stored = next
					return parseErr
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			identities, lists := 0, 0
			d.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "open-api-drive.quark.cn" {
					t.Fatalf("resuming a checkpoint called a token exchange: %s", r.URL.Host)
				}
				if r.URL.Path == "/user" {
					identities++
					id := "tv-account"
					if changed {
						id = "another-account"
					}
					return envelope(r, map[string]any{"user_id": id}), nil
				}
				if r.URL.Path != "/file" || r.URL.Query().Get("method") != "list" {
					t.Fatal("unexpected resumed request")
				}
				lists++
				return envelope(r, map[string]any{"total_count": 0, "files": []any{}}), nil
			})
			_, err = d.List(context.Background(), "0", cloud.PageRequest{Limit: 100})
			if changed {
				errorCode(t, err, CodeIdentity)
				if lists != 0 || stored.TV.IdentityVerified || stored.TV.AccountID != "tv-account" {
					t.Fatal("changed identity became usable or overwrote saved account")
				}
				return
			}
			if err != nil || lists != 1 || identities != 1 || !stored.TV.IdentityVerified || !d.Capabilities().DirectoryList {
				t.Fatalf("actual read did not recover pending checkpoint: %v, identity=%d list=%d", err, identities, lists)
			}
			if err = d.ResumeAuthorizations(context.Background()); err != nil || identities != 1 {
				t.Fatal("verified identity was unnecessarily rechecked")
			}
		})
	}
}

func TestTVAndAgentRefreshSerializeOnePersistedBundle(t *testing.T) {
	stored := fixtureCredentials()
	var mu sync.Mutex
	raw, _ := json.Marshal(stored)
	config := cloud.Config{ConnectionID: 982, Credentials: raw,
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
			stored = next
			return err
		},
	}
	d1, _ := NewClient(config)
	d2, _ := NewClient(config)
	var tokenCalls, inflight, overlap atomic.Int32
	transport := transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "api.extscreen.com" || r.URL.Path == "/agent/v1/oauth/access_token/rotate" {
			tokenCalls.Add(1)
			if inflight.Add(1) != 1 {
				overlap.Add(1)
			}
			time.Sleep(20 * time.Millisecond)
			inflight.Add(-1)
			pair := map[string]any{"access_token": "new-access", "refresh_token": "new-refresh"}
			if r.URL.Host == "api.extscreen.com" {
				pair["status"] = 0
				return response(r, 200, map[string]any{"code": 200, "data": pair}), nil
			}
			return envelope(r, pair), nil
		}
		id := "agent-account"
		if r.URL.Path == "/user" {
			id = "tv-account"
		}
		return envelope(r, map[string]any{"user_id": id}), nil
	})
	d1.api.http.Transport, d2.api.http.Transport = transport, transport
	start, results := make(chan struct{}), make(chan error, 2)
	go func() { <-start; results <- d1.refresh(context.Background(), RouteTV, 1) }()
	go func() { <-start; results <- d2.refresh(context.Background(), RouteAgent, 1) }()
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if overlap.Load() != 0 || tokenCalls.Load() != 2 || stored.Revision != 5 || !stored.TV.IdentityVerified || !stored.Agent.IdentityVerified {
		t.Fatalf("shared bundle rotations overlapped or lost a checkpoint: overlap=%d calls=%d revision=%d", overlap.Load(), tokenCalls.Load(), stored.Revision)
	}
}
