//go:build quark_live

package handlers_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

type liveTVFixture struct {
	dir             string
	brokerCalls     atomic.Int32
	identityCalls   atomic.Int32
	oldValid        atomic.Bool
	identityFailure atomic.Bool
	changedAccount  atomic.Bool
	brokerFailure   atomic.Bool
	brokerDenied    atomic.Bool
}

func newLiveTVFixture(t *testing.T) *liveTVFixture {
	t.Helper()
	f := &liveTVFixture{dir: t.TempDir()}
	key := base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32))
	if err := os.WriteFile(filepath.Join(f.dir, "tv-credentials.key"), []byte(key), 0600); err != nil {
		t.Fatal(err)
	}
	cipher, _ := credential.Open("", key)
	session := liveTVSession{Version: 1, ProfileFingerprint: strings.Repeat("a", 64), DeviceID: strings.Repeat("b", 32), AccessToken: "fixture-old-access", RefreshToken: "fixture-old-refresh", UserID: "fixture-user", Generation: 7, BrokerAuthorization: true, IdentityVerified: true, UpdatedAt: time.Now().UTC()}
	plain, _ := json.Marshal(session)
	ciphertext, _ := cipher.Encrypt(liveTVPurpose, string(plain))
	raw, _ := json.Marshal(map[string]any{"version": 1, "ciphertext": ciphertext})
	if err := os.WriteFile(filepath.Join(f.dir, liveTVSessionName), raw, 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Host == "api.extscreen.com" && r.URL.Path == "/quarkdrive/token":
			f.brokerCalls.Add(1)
			var payload map[string]string
			if r.Method != http.MethodPost || json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&payload) != nil || payload["refresh_token"] != "fixture-old-refresh" || payload["device_id"] != strings.Repeat("b", 32) || payload["code"] != "" {
				t.Error("unexpected broker method or credential scope")
				w.WriteHeader(400)
				return
			}
			// The no-replay marker must be durable before the actual production
			// Authenticator dispatches a potentially consuming broker request.
			if raw, err := liveTVRead(filepath.Join(f.dir, liveTVAttemptName), 16384, false); err != nil || bytes.Contains(raw, []byte("fixture-old-refresh")) {
				t.Error("broker call lacked an encrypted durable attempt")
			}
			if f.brokerDenied.Load() {
				w.WriteHeader(401)
				fmt.Fprint(w, `{"errno":10001}`)
				return
			}
			if f.brokerFailure.Load() {
				w.WriteHeader(503)
				fmt.Fprint(w, `{"message":"fixture-sensitive-response"}`)
				return
			}
			fmt.Fprint(w, `{"code":200,"data":{"status":0,"access_token":"fixture-new-access","refresh_token":"fixture-new-refresh"}}`)
		case r.Host == "open-api-drive.quark.cn" && r.URL.Path == "/user":
			f.identityCalls.Add(1)
			access := r.URL.Query().Get("access_token")
			if access == "fixture-old-access" && !f.oldValid.Load() {
				w.WriteHeader(401)
				fmt.Fprint(w, `{"errno":10001}`)
				return
			}
			if access == "fixture-new-access" && f.identityFailure.Load() {
				w.WriteHeader(503)
				fmt.Fprint(w, `{"message":"fixture-sensitive-response"}`)
				return
			}
			if access != "fixture-old-access" && access != "fixture-new-access" {
				t.Error("unexpected identity credential")
			}
			account := "fixture-user"
			if access == "fixture-new-access" && f.changedAccount.Load() {
				account = "fixture-other-user"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": 0, "data": map[string]string{"user_id": account}})
		default:
			t.Error("unexpected network destination in bounded fixture")
			w.WriteHeader(400)
		}
	}))
	t.Cleanup(server.Close)
	// This transport can dial only the local TLS fixture. Native Authenticator
	// still constructs its real pinned broker and first-party request shapes.
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.ServerName = server.Certificate().DNSNames[0]
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previous; transport.CloseIdleConnections() })
	return f
}

func (f *liveTVFixture) open(t *testing.T) *liveTVStore {
	t.Helper()
	s, err := openLiveTVStore(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.lock.Close() })
	return s
}

func readyLiveTV(s *liveTVStore) (quark.Credentials, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return s.ready(ctx, quark.NewAuthenticator())
}

func TestQuarkLiveTVRefreshPersistsAndReloadsWithoutRepeat(t *testing.T) {
	f := newLiveTVFixture(t)
	s := f.open(t)
	originalKey := append([]byte{}, s.key...)
	c, refreshed, err := readyLiveTV(s)
	if err != nil || !refreshed || c.TV == nil || c.TV.AccessToken != "fixture-new-access" || c.TV.RefreshToken != "" || c.TV.Generation != 8 || !c.TV.IdentityVerified {
		t.Fatalf("startup recovery failed: refreshed=%v err=%v", refreshed, err)
	}
	if s.session.RefreshToken != "fixture-new-refresh" || !s.session.BrokerRefresh || !bytes.Equal(originalKey, s.key) || s.unchanged() != nil {
		t.Fatal("original store was not safely rotated")
	}
	if _, err := os.Stat(filepath.Join(f.dir, liveTVCheckpointName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("committed checkpoint was not retired")
	}
	if _, err := os.Stat(filepath.Join(f.dir, liveTVAttemptName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("committed attempt was not retired")
	}
	_ = s.lock.Close()
	reloaded := f.open(t)
	if _, rotated, err := readyLiveTV(reloaded); err != nil || rotated || f.brokerCalls.Load() != 1 {
		t.Fatalf("reload consumed another refresh: rotated=%v err=%v", rotated, err)
	}
}

func TestQuarkLiveTVValidSessionIsByteUnchanged(t *testing.T) {
	f := newLiveTVFixture(t)
	f.oldValid.Store(true)
	s := f.open(t)
	before := append([]byte{}, s.saved...)
	if c, renewed, err := readyLiveTV(s); err != nil || renewed || c.TV.RefreshToken != "" || f.brokerCalls.Load() != 0 || !bytes.Equal(before, s.saved) {
		t.Fatalf("valid session unexpectedly mutated: renewed=%v err=%v", renewed, err)
	}
}

func TestQuarkLiveTVCheckpointResumesWithoutConsumingOldToken(t *testing.T) {
	for _, failure := range []string{"identity_unavailable", "identity_changed", "main_save"} {
		t.Run(failure, func(t *testing.T) {
			f := newLiveTVFixture(t)
			s := f.open(t)
			before := append([]byte{}, s.saved...)
			if failure == "identity_unavailable" {
				f.identityFailure.Store(true)
			} else if failure == "identity_changed" {
				f.changedAccount.Store(true)
			} else {
				s.write = func(name string, raw []byte) error {
					if name == liveTVSessionName {
						return errors.New("fixture save refusal")
					}
					return s.atomicWrite(name, raw)
				}
			}
			if _, _, err := readyLiveTV(s); err == nil || s.checkpoint == nil || !bytes.Equal(before, s.saved) || f.brokerCalls.Load() != 1 {
				t.Fatalf("failed confirmation changed main or lost recovery: %v", err)
			} else if strings.Contains(liveTVMessage(err), "fixture") {
				t.Fatal("sensitive response was exposed")
			}
			_ = s.lock.Close()
			reloaded := f.open(t)
			f.identityFailure.Store(false)
			f.changedAccount.Store(false)
			if _, renewed, err := readyLiveTV(reloaded); err != nil || !renewed || f.brokerCalls.Load() != 1 || reloaded.session.Generation != 8 {
				t.Fatalf("checkpoint did not resume exactly once: %v", err)
			}
		})
	}
}

func TestQuarkLiveTVAmbiguousAndDeniedRefreshDoNotReplay(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(fmt.Sprint(denied), func(t *testing.T) {
			f := newLiveTVFixture(t)
			f.brokerFailure.Store(!denied)
			f.brokerDenied.Store(denied)
			s := f.open(t)
			before := append([]byte{}, s.saved...)
			want := "tv_refresh_uncertain"
			if denied {
				want = "tv_refresh_denied"
			}
			if _, _, err := readyLiveTV(s); err == nil || err.Error() != want || !bytes.Equal(before, s.saved) {
				t.Fatalf("unsafe failed refresh: %v", err)
			}
			_ = s.lock.Close()
			reloaded := f.open(t)
			if _, _, err := readyLiveTV(reloaded); err == nil || err.Error() != want || f.brokerCalls.Load() != 1 {
				t.Fatalf("ambiguous refresh was replayed: %v", err)
			}
		})
	}
}

func TestQuarkLiveTVWritesMustPrecedeNetworkAndIdentity(t *testing.T) {
	for _, blocked := range []string{liveTVAttemptName, liveTVCheckpointName} {
		t.Run(blocked, func(t *testing.T) {
			f := newLiveTVFixture(t)
			s := f.open(t)
			s.write = func(name string, raw []byte) error {
				if name == blocked {
					return errors.New("fixture write error")
				}
				return s.atomicWrite(name, raw)
			}
			if _, _, err := readyLiveTV(s); err == nil {
				t.Fatal("failed persistence accepted")
			}
			if blocked == liveTVAttemptName && f.brokerCalls.Load() != 0 || blocked == liveTVCheckpointName && (f.brokerCalls.Load() != 1 || f.identityCalls.Load() != 1) {
				t.Fatal("network advanced past failed durable boundary")
			}
			_ = s.lock.Close()
			if blocked == liveTVCheckpointName {
				if _, _, err := readyLiveTV(f.open(t)); err == nil || err.Error() != "tv_refresh_uncertain" || f.brokerCalls.Load() != 1 {
					t.Fatal("lost saved response consumed old refresh again")
				}
			}
		})
	}
}

func TestQuarkLiveTVOriginalRuntimeLockAndMutationFence(t *testing.T) {
	f := newLiveTVFixture(t)
	s := f.open(t)
	if concurrent, err := database.AcquireExclusiveRuntime(filepath.Join(f.dir, liveTVSessionName)); err == nil {
		concurrent.Close()
		t.Fatal("legacy helper can race renewal")
	}
	if concurrent, err := openLiveTVStore(f.dir); err == nil || err.Error() != "tv_state_locked" {
		if concurrent != nil {
			concurrent.lock.Close()
		}
		t.Fatal("second validator acquired rotating state")
	}
	if err := os.WriteFile(filepath.Join(f.dir, liveTVSessionName), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readyLiveTV(s); err == nil || err.Error() != "tv_state_changed" || f.brokerCalls.Load() != 0 || f.identityCalls.Load() != 0 {
		t.Fatal("changed source state reached account APIs")
	}
}
