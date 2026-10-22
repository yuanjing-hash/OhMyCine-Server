//go:build quark_live

package handlers_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

// This builds only synthetic encrypted predecessor receipts in t.TempDir. It
// does not claim that the predecessor's real service or cloud operation ran.
func retirementIndependentParent(t *testing.T) (*followParent, *retirementParent) {
	t.Helper()
	creation, previous := followIndependentStore(t)
	_, _, _ = followIndependentRequest(t, previous, "rename")
	_, _, _ = followIndependentRequest(t, previous, "move")
	for _, pair := range []struct{ id, kind string }{{previous.state.RenameID, "rename"}, {previous.state.DirectoryID, "mkdir"}, {previous.state.MoveID, "move"}} {
		r := previous.state.Operations[pair.id]
		var p followOperation
		if json.Unmarshal(r.Payload, &p) != nil {
			t.Fatal("synthetic predecessor payload")
		}
		r.State, r.Revision = "confirmed", 2
		if p.Mapping != nil {
			p.Mapping.ReceiptRevision = r.Revision
		}
		if pair.kind != "mkdir" {
			parent := previous.state.Identity.RootAgent
			if pair.kind == "move" {
				parent = previous.state.NewDirectoryAgent
			}
			p.Outputs = []cloud.Item{{ID: quark.QualifyID(quark.RouteAgent, previous.state.Identity.AgentAccount, previous.state.Identity.SampleAgent), ParentID: quark.QualifyID(quark.RouteAgent, previous.state.Identity.AgentAccount, parent), Name: previous.state.RenameName, Size: observationFixtureSize}}
		}
		r.Payload, _ = json.Marshal(p)
		previous.state.Operations[pair.id] = r
		previous.state.Writes[pair.id+":"+pair.kind] = true
	}
	previous.state.Completed = true
	if err := previous.save(); err != nil {
		t.Fatal(err)
	}
	journalPath, keyPath := filepath.Join(previous.dir, "followon.enc"), filepath.Join(previous.dir, "followon.key")
	journal, e := os.ReadFile(journalPath)
	if e != nil {
		t.Fatal(e)
	}
	key, e := os.ReadFile(keyPath)
	if e != nil {
		t.Fatal(e)
	}
	id, e := retirementIdentityFromState(previous.state, journal, key, []byte("synthetic-service-key"))
	if e != nil {
		t.Fatal("valid synthetic predecessor rejected")
	}
	return creation, &retirementParent{store: previous, identity: id, files: map[string][]byte{journalPath: journal, keyPath: key}}
}

func retirementIndependentStore(t *testing.T) (*followParent, *retirementParent, *retirementStore) {
	t.Helper()
	c, p := retirementIndependentParent(t)
	s, err := openRetirementStore(observationOfflinePrivateDir(t), p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.lock.Close() })
	return c, p, s
}

func retirementIndependentEditReceipt(t *testing.T, st *followState, id string, edit func(*cloud.OperationReceipt, *followOperation)) {
	t.Helper()
	r := st.Operations[id]
	var p followOperation
	if json.Unmarshal(r.Payload, &p) != nil {
		t.Fatal("synthetic payload")
	}
	edit(&r, &p)
	r.Payload, _ = json.Marshal(p)
	st.Operations[id] = r
}

func TestQuarkObservationRetirementIndependentPredecessor(t *testing.T) {
	creation, parent := retirementIndependentParent(t)
	raw, _ := json.Marshal(parent.store.state)
	for _, mode := range []string{"valid", "not_completed", "unverified_content", "unknown_rename", "unknown_move", "missing_dispatch", "foreign_item", "foreign_parent", "wrong_name", "wrong_size", "extra_output", "wrong_namespace", "outside_proof", "foreign_inside_parent"} {
		t.Run(mode, func(t *testing.T) {
			var st followState
			if json.Unmarshal(raw, &st) != nil {
				t.Fatal("synthetic state clone")
			}
			switch mode {
			case "not_completed":
				st.Completed = false
			case "unverified_content":
				st.InitialContentVerified = false
			case "missing_dispatch":
				delete(st.Writes, st.MoveID+":move")
			case "outside_proof":
				st.Identity.Root.ParentID = quark.QualifyID(quark.RouteTV, st.Identity.TVAccount, "foreign-root")
			case "foreign_inside_parent":
				st.NewDirectory.ParentID = st.Identity.Proof.TVRoot
			case "valid":
			default:
				id := st.MoveID
				if mode == "unknown_rename" {
					id = st.RenameID
				}
				retirementIndependentEditReceipt(t, &st, id, func(r *cloud.OperationReceipt, p *followOperation) {
					switch mode {
					case "unknown_rename", "unknown_move":
						r.State = "unknown"
					case "foreign_item":
						p.Outputs[0].ID = quark.QualifyID(quark.RouteAgent, st.Identity.AgentAccount, "other-sample")
					case "foreign_parent":
						p.Outputs[0].ParentID = quark.QualifyID(quark.RouteAgent, st.Identity.AgentAccount, "other-parent")
					case "wrong_name":
						p.Outputs[0].Name = "another.mp4"
					case "wrong_size":
						p.Outputs[0].Size++
					case "extra_output":
						p.Outputs = append(p.Outputs, p.Outputs[0])
					case "wrong_namespace":
						p.Outputs[0].ID = quark.QualifyID(quark.RouteTV, st.Identity.TVAccount, st.Identity.SampleAgent)
					}
				})
			}
			_, err := retirementIdentityFromState(st, []byte("synthetic-journal"), []byte("synthetic-key"), []byte("synthetic-service-key"))
			if (err == nil) != (mode == "valid") {
				t.Fatalf("unexpected predecessor acceptance for %s", mode)
			}
		})
	}
	if creation.unchanged() != nil || parent.unchanged() != nil {
		t.Fatal("predecessor validation modified historical evidence")
	}
}

func TestQuarkObservationRetirementIndependentPersistentBudget(t *testing.T) {
	creation, parent, s := retirementIndependentStore(t)
	s.state.Requests, s.state.API = 7, 6
	s.state.LastDispatch = time.Now().UTC()
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	before := s.state
	_ = s.lock.Close()
	reopened, err := openRetirementStore(s.dir, parent)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reopened.state, before) {
		t.Fatal("resume reset persisted identity, budget or deadline")
	}
	if _, err = openRetirementStore(s.dir, parent); err == nil {
		t.Fatal("second runtime acquired active batch")
	}
	_ = reopened.lock.Close()
	changed := *parent
	changed.identity.PredecessorJournalSHA = "different-journal"
	if _, err = openRetirementStore(s.dir, &changed); err == nil {
		t.Fatal("different predecessor resumed batch")
	}
	if creation.unchanged() != nil || parent.unchanged() != nil {
		t.Fatal("resume modified predecessor evidence")
	}
	// Test-only aging of the synthetic journal verifies refusal, not live timing.
	s.state.Started = time.Now().UTC().Add(-26 * time.Minute)
	s.state.Deadline = s.state.Started.Add(retirementDuration)
	if err = s.save(); err != nil {
		t.Fatal(err)
	}
	old, _ := os.ReadFile(filepath.Join(s.dir, "retirement.enc"))
	if _, err = openRetirementStore(s.dir, parent); err == nil {
		t.Fatal("expired batch gained a new budget")
	}
	after, _ := os.ReadFile(filepath.Join(s.dir, "retirement.enc"))
	if !bytes.Equal(old, after) {
		t.Fatal("expired batch refusal rewrote journal")
	}
}

func retirementIndependentMove(t *testing.T, s *retirementStore, back bool) (map[string]any, string) {
	t.Helper()
	id := s.state.MoveOutID
	if back {
		_, _ = retirementIndependentMove(t, s, false)
		out := s.state.Operations[s.state.MoveOutID]
		out.State = "confirmed"
		s.state.Operations[out.ID] = out
		id = s.state.MoveBackID
	}
	pre, ok := retirementExpectedPreflight(s.state, id)
	if !ok {
		t.Fatal("synthetic move scope")
	}
	pre.At = time.Now().UTC()
	s.state.Preflights[id] = pre
	i := s.state.Identity.Creation
	p := followOperation{Request: cloud.OperationRequest{ID: id, Kind: "move", ItemID: quark.QualifyID(quark.RouteAgent, i.AgentAccount, i.SampleAgent), ParentID: quark.QualifyID(quark.RouteAgent, i.AgentAccount, pre.TargetAgent)}}
	p.Before.ID, p.Before.Parent, p.Before.Name, p.Before.Size = i.SampleAgent, pre.SourceAgent, pre.Name, observationFixtureSize
	raw, _ := json.Marshal(p)
	s.state.Operations[id] = cloud.OperationReceipt{ID: id, Kind: "move", Provider: cloud.ProviderQuark, Route: string(quark.RouteAgent), AccountID: i.AgentAccount, State: "submitting", Revision: 1, Payload: raw}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	return map[string]any{"fid_list": []string{i.SampleAgent}, "to_pdir_fid": pre.TargetAgent, "action_type": 1}, id
}

func TestQuarkObservationRetirementIndependentExactMoveGuard(t *testing.T) {
	for _, back := range []bool{false, true} {
		for _, mode := range []string{"valid", "foreign_file", "extra_file", "foreign_target", "missing_preflight", "stale_preflight", "future_preflight", "wrong_preflight", "unknown_receipt", "already_dispatched", "wrong_before", "completed", "expired", "api_budget", "total_budget", "back_before_out"} {
			if mode == "back_before_out" && !back {
				continue
			}
			t.Run(map[bool]string{false: "out", true: "back"}[back]+"/"+mode, func(t *testing.T) {
				creation, parent, s := retirementIndependentStore(t)
				body, id := retirementIndependentMove(t, s, back)
				switch mode {
				case "foreign_file":
					body["fid_list"] = []string{"foreign-file"}
				case "extra_file":
					body["fid_list"] = []string{s.state.Identity.Creation.SampleAgent, "extra"}
				case "foreign_target":
					body["to_pdir_fid"] = "foreign-target"
				case "missing_preflight":
					delete(s.state.Preflights, id)
				case "stale_preflight", "future_preflight", "wrong_preflight":
					p := s.state.Preflights[id]
					if mode == "stale_preflight" {
						p.At = time.Now().Add(-2 * time.Minute)
					} else if mode == "future_preflight" {
						p.At = time.Now().Add(time.Minute)
					} else {
						p.SourceAgent = "other-parent"
					}
					s.state.Preflights[id] = p
				case "unknown_receipt", "wrong_before":
					r := s.state.Operations[id]
					if mode == "unknown_receipt" {
						r.State = "unknown"
					} else {
						var p followOperation
						_ = json.Unmarshal(r.Payload, &p)
						p.Before.Parent = "other-parent"
						r.Payload, _ = json.Marshal(p)
					}
					s.state.Operations[id] = r
				case "already_dispatched":
					s.state.Writes[id+":move"] = true
				case "completed":
					s.state.Completed = true
				case "expired":
					s.state.Deadline = time.Now().Add(-time.Second)
				case "api_budget":
					s.state.Requests, s.state.API = 200, 200
				case "total_budget":
					s.state.Requests, s.state.API = 220, 199
				case "back_before_out":
					r := s.state.Operations[s.state.MoveOutID]
					r.State = "unknown"
					s.state.Operations[r.ID] = r
				}
				calls, initial := 0, s.state.Requests
				g := newRetirementGuard(s)
				g.delay = 0
				transport := g.wrap(observationOfflineTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if !s.state.Writes[id+":move"] || s.state.Requests != initial+1 {
						t.Fatal("write dispatch preceded durable accounting")
					}
					sealed, e := os.ReadFile(filepath.Join(s.dir, "retirement.enc"))
					if e != nil {
						t.Fatal(e)
					}
					plain, e := s.cipher.Decrypt(retirementPurpose, string(sealed))
					var persisted retirementState
					if e != nil || json.Unmarshal([]byte(plain), &persisted) != nil || !persisted.Writes[id+":move"] || persisted.Requests != initial+1 {
						t.Fatal("write reached transport before encrypted journal checkpoint")
					}
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(`{"status":0,"data":{}}`)), Request: r}, nil
				}), false)
				response, err := transport.RoundTrip(followIndependentHTTP("/open/v1/file/move", body))
				if response != nil {
					_ = response.Body.Close()
				}
				if mode == "valid" {
					if err != nil || calls != 1 {
						t.Fatal("exact frozen move rejected")
					}
					if _, err = transport.RoundTrip(followIndependentHTTP("/open/v1/file/move", body)); err == nil || calls != 1 {
						t.Fatal("same write dispatched twice")
					}
				} else if err == nil || calls != 0 || s.state.Requests != initial {
					t.Fatalf("unsafe move dispatched: %s", mode)
				}
				if creation.unchanged() != nil || parent.unchanged() != nil {
					t.Fatal("guard changed predecessor evidence")
				}
			})
		}
	}
}

func TestQuarkObservationRetirementIndependentForbiddenEndpoints(t *testing.T) {
	_, _, s := retirementIndependentStore(t)
	g := newRetirementGuard(s)
	g.delay, g.startup = 0, false
	calls := 0
	base := observationOfflineTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected transport") })
	for _, endpoint := range []string{"/open/v1/dir", "/open/v1/file/rename/batch", "/open/v1/file/upload/pre", "/open/v1/file/upload/finish", "/open/v1/file/copy", "/open/v1/file/delete", "/open/v1/file/recycle", "/file?method=download", "/open/v1/file/query_task?task_id=unknown"} {
		if _, err := g.wrap(base, false).RoundTrip(followIndependentHTTP(endpoint, map[string]any{})); err == nil {
			t.Fatalf("unexpected endpoint accepted: %s", endpoint)
		}
	}
	media, _ := http.NewRequest("GET", "https://cdn.example.invalid/synthetic", nil)
	if _, err := g.wrap(base, true).RoundTrip(media); err == nil || calls != 0 || s.state.Requests != 0 {
		t.Fatal("retirement read media or sent forbidden request")
	}
}

func TestQuarkObservationRetirementIndependentDispatchTimeBounds(t *testing.T) {
	t.Run("preflight_expires_during_queue_delay", func(t *testing.T) {
		_, _, s := retirementIndependentStore(t)
		body, id := retirementIndependentMove(t, s, false)
		p := s.state.Preflights[id]
		p.At = time.Now().Add(-time.Minute + 100*time.Millisecond)
		s.state.Preflights[id] = p
		s.state.LastDispatch = time.Now()
		g := newRetirementGuard(s)
		g.delay = 200 * time.Millisecond
		calls := 0
		w := g.wrap(observationOfflineTransport(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("unexpected expired preflight dispatch")
		}), false)
		if _, err := w.RoundTrip(followIndependentHTTP("/open/v1/file/move", body)); err == nil || calls != 0 || s.state.Requests != 0 {
			t.Fatal("queued request bypassed dispatch-time preflight expiry")
		}
	})
	t.Run("request_inherits_original_deadline", func(t *testing.T) {
		_, _, s := retirementIndependentStore(t)
		body, _ := retirementIndependentMove(t, s, false)
		s.state.Deadline = time.Now().Add(time.Second)
		deadline := s.state.Deadline
		g := newRetirementGuard(s)
		g.delay = 0
		calls := 0
		w := g.wrap(observationOfflineTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			actual, ok := r.Context().Deadline()
			if !ok || actual.After(deadline) {
				t.Fatal("request deadline exceeded original batch")
			}
			return observationOfflineResponse(`{"status":0,"data":{}}`), nil
		}), false)
		resp, err := w.RoundTrip(followIndependentHTTP("/open/v1/file/move", body))
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err != nil || calls != 1 {
			t.Fatal("exact bounded request rejected")
		}
	})
	t.Run("lost_response_cold_guard_cannot_resubmit", func(t *testing.T) {
		_, parent, s := retirementIndependentStore(t)
		body, id := retirementIndependentMove(t, s, false)
		g := newRetirementGuard(s)
		g.delay = 0
		calls := 0
		base := observationOfflineTransport(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("synthetic transport failure with private response")
		})
		if _, err := g.wrap(base, false).RoundTrip(followIndependentHTTP("/open/v1/file/move", body)); err == nil || err.Error() != observationError().Error() || calls != 1 {
			t.Fatal("transport failure did not remain unknown and redacted")
		}
		_ = s.lock.Close()
		reopened, err := openRetirementStore(s.dir, parent)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.lock.Close()
		cold := newRetirementGuard(reopened)
		cold.delay = 0
		if !reopened.state.Writes[id+":move"] || reopened.state.Requests != 1 {
			t.Fatal("restart lost durable dispatched evidence")
		}
		if _, err = cold.wrap(base, false).RoundTrip(followIndependentHTTP("/open/v1/file/move", body)); err == nil || calls != 1 || reopened.state.Requests != 1 {
			t.Fatal("cold guard repeated a possibly successful move")
		}
	})
}
