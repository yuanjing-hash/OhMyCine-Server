//go:build quark_live

package handlers_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

func TestQuarkObservationAgentProfileUsesOriginalOptionalPlatform(t *testing.T) {
	// Published application profile constants, not an account credential. Pin
	// the old authorization helper's raw optional-platform hash independently
	// from the implementation so a request-time default cannot change identity.
	fingerprint := func(client, sign, platform string) string {
		h := sha256.Sum256([]byte(client + "\x00" + sign + "\x00" + platform))
		return hex.EncodeToString(h[:])
	}
	const client = "third_party_agent"
	const sign = "cf134812e2de4032bd1cb7c3727e84b3"
	expected := fingerprint(client, sign, "")
	if got := quark.ObservationAgentProfileFingerprint(); got != expected {
		t.Fatal("observation helper changed the original optional-platform profile identity")
	}
	for _, tc := range []struct{ name, app string }{
		{"request_platform_web", fingerprint(client, sign, "web")},
		{"different_client", fingerprint("another-public-client", sign, "")},
		{"different_sign_profile", fingerprint(client, "another-public-sign-profile", "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.app == expected {
				t.Fatal("negative fixture equals expected profile")
			}
			dir, original := observationOfflineAgent(t)
			original.lock.Close()
			key, err := os.ReadFile(filepath.Join(dir, "credentials.key"))
			if err != nil {
				t.Fatal("synthetic key read failed")
			}
			cipher, err := credential.Open("", string(key))
			if err != nil {
				t.Fatal("synthetic key open failed")
			}
			// Both encrypted files agree with each other but differ from the
			// pinned application. Ledger/session equality alone must not pass.
			original.session.App, original.ledger.App = tc.app, tc.app
			for _, v := range []struct {
				name, purpose string
				value         any
			}{
				{"session.enc.json", "quark-original-302-probe:state:v1", original.session},
				{"capability-ledger.enc.json", "quark-capability-validation:ownership-intent:v1", original.ledger},
			} {
				raw, err := json.Marshal(v.value)
				if err != nil {
					t.Fatal("synthetic encoding failed")
				}
				sealed, err := cipher.Encrypt(v.purpose, string(raw))
				if err != nil {
					t.Fatal("synthetic encryption failed")
				}
				envelope, err := json.Marshal(struct {
					Version    int
					Ciphertext string
				}{1, sealed})
				if err != nil || os.WriteFile(filepath.Join(dir, v.name), envelope, 0600) != nil {
					t.Fatal("synthetic encrypted state write failed")
				}
			}
			got, err := openObservationAgent(dir)
			if got != nil {
				got.lock.Close()
				t.Fatal("changed application profile was accepted")
			}
			var local observationLocalFailure
			if !errors.As(err, &local) || local != "agent_profile_mismatch" {
				t.Fatal("changed profile did not produce the exact safe local failure")
			}
			lock, err := database.AcquireExclusiveRuntime(filepath.Join(dir, "session.enc.json"))
			if err != nil {
				t.Fatal("profile rejection left original state locked")
			}
			lock.Close()
		})
	}
}

func TestQuarkObservationUploadACKAdmitsOnlyOneMatchingParentInfo(t *testing.T) {
	for _, tc := range []struct {
		name, responseID, responseName, responseType, requestID string
		size                                                    int64
		badReceipt                                              bool
		allowed                                                 bool
	}{
		{"matching", "upload-ack", "unique.mp4", "1", "upload-ack", observationFixtureSize, false, true},
		{"wrong_size", "upload-ack", "unique.mp4", "1", "upload-ack", observationFixtureSize + 1, false, false},
		{"wrong_name", "upload-ack", "foreign.mp4", "1", "upload-ack", observationFixtureSize, false, false},
		{"wrong_type", "upload-ack", "unique.mp4", "0", "upload-ack", observationFixtureSize, false, false},
		{"wrong_response_ack", "foreign-ack", "unique.mp4", "1", "upload-ack", observationFixtureSize, false, false},
		{"unknown_request_ack", "foreign-ack", "unique.mp4", "1", "foreign-ack", observationFixtureSize, false, false},
		{"invalid_receipt", "upload-ack", "unique.mp4", "1", "upload-ack", observationFixtureSize, true, false},
		{"upstream_error_status", "upload-ack", "unique.mp4", "1", "upload-ack", observationFixtureSize, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := observationOfflineStore(t)
			p := map[string]any{"parent": "child-listed", "name": s.state.UploadName, "size": observationFixtureSize, "sha256": observationFixtureSHA, "handle": "upload-ack", "phase": "confirming"}
			if tc.badReceipt {
				p["sha256"] = "unrelated-digest"
			}
			raw, _ := json.Marshal(p)
			s.state.Operations[s.state.UploadID] = cloud.OperationReceipt{ID: s.state.UploadID, Kind: "upload", Provider: cloud.ProviderQuark, Route: "agent", AccountID: s.state.AgentAccount, Revision: 1, State: "accepted", Payload: raw}
			g := newObservationGuard(s)
			g.delay = 0
			g.childAgent = "child-listed"
			calls := 0
			w := g.wrap(observationOfflineTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				f := map[string]any{"fid": tc.responseID, "filename": tc.responseName, "file_type": tc.responseType, "size": tc.size, "parent_fid": "ack-parent"}
				if r.URL.Query().Get("fid") == "ack-parent" {
					f = map[string]any{"fid": "ack-parent", "filename": "parent", "file_type": "0", "parent_fid": "second-ancestor"}
				}
				body, _ := json.Marshal(map[string]any{"data": f})
				resp := observationOfflineResponse(string(body))
				if tc.name == "upstream_error_status" {
					resp.StatusCode = http.StatusServiceUnavailable
				}
				return resp, nil
			}), false)
			get := func(id string) error {
				r, _ := http.NewRequest("GET", "https://open-api-drive.quark.cn/open/v1/file/info?fid="+id, nil)
				resp, err := w.RoundTrip(r)
				if resp != nil {
					resp.Body.Close()
				}
				return err
			}
			firstErr := get(tc.requestID)
			if tc.requestID == "foreign-ack" && (firstErr == nil || calls != 0) {
				t.Fatal("unknown ACK reached transport")
			}
			parentErr := get("ack-parent")
			if (parentErr == nil) != tc.allowed {
				t.Fatal("parent info scope did not match exact receipt evidence")
			}
			before := calls
			list, _ := http.NewRequest("POST", "https://open-api-drive.quark.cn/open/v1/file/list", strings.NewReader(`{"parent_fid":"ack-parent","size":100}`))
			if resp, err := w.RoundTrip(list); err == nil {
				if resp != nil {
					resp.Body.Close()
				}
				t.Fatal("parent metadata permission expanded to directory list")
			}
			if get("second-ancestor") == nil || calls != before {
				t.Fatal("parent metadata permission followed an ancestor chain")
			}
			if s.state.PreWrites != 0 || s.state.PutWrites != 0 || s.state.FinishWrites != 0 || s.state.DirectoryWrites != 0 {
				t.Fatal("metadata verification performed a write")
			}
		})
	}
}
