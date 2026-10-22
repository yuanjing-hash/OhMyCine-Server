//go:build quark_live

package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

// All account values and receipts here are synthetic. The original-store
// fixture is encrypted under a disposable key and must remain byte-identical.
func followIndependentParent(t *testing.T) *followParent {
	t.Helper()
	s := observationOfflineStore(t)
	st := &s.state
	n := strings.Repeat("c", 32)
	st.Nonce = n
	st.ProofID = "observation-proof-" + n
	st.DirectoryID = "observation-directory-" + n
	st.UploadID = "observation-video-" + n
	st.DirectoryName = "native-observation-" + n
	st.UploadName = "omc-synthetic-" + n + ".mp4"
	st.Completed = true
	proofRoot := quark.QualifyID(quark.RouteTV, st.TVAccount, "proof-root")
	root := cloud.Item{ID: quark.QualifyID(quark.RouteTV, st.TVAccount, "owned-root"), ParentID: proofRoot, Name: st.DirectoryName, IsDir: true}
	sample := cloud.Item{ID: quark.QualifyID(quark.RouteTV, st.TVAccount, "owned-sample"), ParentID: root.ID, Name: st.UploadName, Size: observationFixtureSize}
	p := quark.SpaceProof{Version: 1, Generation: 1, TVAccount: st.TVAccount, AgentAccount: st.AgentAccount, TVRoot: proofRoot, AgentRoot: quark.QualifyID(quark.RouteAgent, st.AgentAccount, "proof-listed"), Path: "/" + st.RootName, ReceiptID: st.ProofID, ContentSHA256: strings.Repeat("a", 64), VerifiedAt: time.Now().UTC()}
	st.Bundle.Proofs = []quark.SpaceProof{p}
	put := func(id, kind string, payload any) {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		st.Operations[id] = cloud.OperationReceipt{ID: id, Kind: kind, Provider: cloud.ProviderQuark, Route: string(quark.RouteAgent), AccountID: st.AgentAccount, Revision: 2, State: "confirmed", Payload: raw}
	}
	put(st.ProofID, "space_proof", map[string]any{"proof": p})
	put(st.ProofID+"/upload", "upload", map[string]any{"handle": "proof-ack"})
	put(st.DirectoryID, "mkdir", map[string]any{"handle": "root-ack", "mapping": followMapping{Version: 1, Operation: st.DirectoryID, ReceiptRevision: 2, ACK: "root-ack", Listed: "root-listed", TV: root}, "target": followTarget{Proof: p, Path: p.Path, AgentParent: "proof-listed", TVParent: proofRoot}})
	put(st.UploadID, "upload", map[string]any{"parent": "root-listed", "name": st.UploadName, "handle": "sample-ack", "size": observationFixtureSize, "sha256": observationFixtureSHA, "mapping": followMapping{Version: 1, Operation: st.UploadID, ReceiptRevision: 2, ACK: "sample-ack", Listed: "sample-listed", TV: sample, SHA256: observationFixtureSHA}, "target": followTarget{Proof: p, Path: p.Path + "/" + st.DirectoryName, AgentParent: "root-listed", TVParent: root.ID}})
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	journal, err := os.ReadFile(filepath.Join(s.dir, "observation.enc"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(filepath.Join(s.dir, "observation.key"))
	if err != nil {
		t.Fatal(err)
	}
	id, err := followIdentityFromState(s.state, journal, key)
	if err != nil {
		t.Fatal("synthetic parent receipts rejected")
	}
	return &followParent{store: s, identity: id, files: map[string][]byte{filepath.Join(s.dir, "observation.enc"): journal, filepath.Join(s.dir, "observation.key"): key}}
}

func followIndependentStore(t *testing.T) (*followParent, *followStore) {
	t.Helper()
	p := followIndependentParent(t)
	s, err := openFollowStore(observationOfflinePrivateDir(t), p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.lock.Close() })
	raw, _ := json.Marshal(p.store.state.Bundle)
	if json.Unmarshal(raw, &s.state.Bundle) != nil {
		t.Fatal("synthetic bundle clone")
	}
	s.state.InitialContentVerified = true
	if err = s.save(); err != nil {
		t.Fatal(err)
	}
	return p, s
}

func followIndependentRequest(t *testing.T, s *followStore, kind string) (string, map[string]any, string) {
	t.Helper()
	i := s.state.Identity
	id, endpoint := s.state.RenameID, "/open/v1/file/rename/batch"
	p := followOperation{Request: cloud.OperationRequest{ID: id, Kind: kind, ItemID: quark.QualifyID(quark.RouteAgent, i.AgentAccount, i.SampleAgent), Name: s.state.RenameName}}
	p.Before.ID = i.SampleAgent
	p.Before.Parent = i.RootAgent
	p.Before.Name = i.Sample.Name
	p.Before.Size = observationFixtureSize
	p.BatchID = "0123456789abcdef"
	body := map[string]any{"batch_id": p.BatchID, "operation": "RENAME", "rename_list": []map[string]string{{"fid": i.SampleAgent, "file_name": s.state.RenameName, "origin_file_name": i.Sample.Name}}}
	if kind == "mkdir" {
		id, endpoint = s.state.DirectoryID, "/open/v1/dir"
		p.Request = cloud.OperationRequest{ID: id, Kind: kind, ParentID: quark.QualifyID(quark.RouteAgent, i.AgentAccount, i.RootAgent), Name: s.state.DirectoryName}
		body = map[string]any{"dir_path": s.state.DirectoryName, "pdir_fid": i.RootAgent}
	}
	if kind == "move" {
		_, _, _ = followIndependentRequest(t, s, "mkdir")
		r := s.state.Operations[s.state.DirectoryID]
		r.State = "confirmed"
		s.state.NewDirectory = cloud.Item{ID: quark.QualifyID(quark.RouteTV, i.TVAccount, "new-owned-directory"), ParentID: i.Root.ID, Name: s.state.DirectoryName, IsDir: true}
		s.state.NewDirectoryAgent = "new-directory-listed"
		var directory followOperation
		_ = json.Unmarshal(r.Payload, &directory)
		directory.Handle = "new-directory-ack"
		directory.Mapping = &followMapping{Version: 1, Operation: r.ID, ReceiptRevision: r.Revision, ACK: directory.Handle, Listed: s.state.NewDirectoryAgent, TV: s.state.NewDirectory}
		r.Payload, _ = json.Marshal(directory)
		s.state.Operations[s.state.DirectoryID] = r
		id, endpoint = s.state.MoveID, "/open/v1/file/move"
		p.Request = cloud.OperationRequest{ID: id, Kind: kind, ItemID: quark.QualifyID(quark.RouteAgent, i.AgentAccount, i.SampleAgent), ParentID: quark.QualifyID(quark.RouteAgent, i.AgentAccount, s.state.NewDirectoryAgent)}
		p.Before.Name = s.state.RenameName
		body = map[string]any{"fid_list": []string{i.SampleAgent}, "to_pdir_fid": s.state.NewDirectoryAgent, "action_type": 1}
	}
	raw, _ := json.Marshal(p)
	s.state.Operations[id] = cloud.OperationReceipt{ID: id, Kind: kind, Provider: cloud.ProviderQuark, Route: string(quark.RouteAgent), AccountID: i.AgentAccount, Revision: 1, State: "submitting", Payload: raw}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	return endpoint, body, id + ":" + kind
}

func followIndependentHTTP(path string, body map[string]any) *http.Request {
	raw, _ := json.Marshal(body)
	r, _ := http.NewRequest("POST", "https://open-api-drive.quark.cn"+path, bytes.NewReader(raw))
	return r
}

func TestQuarkObservationFollowonIndependentExactWriteGuard(t *testing.T) {
	for _, kind := range []string{"rename", "mkdir", "move"} {
		for _, mode := range []string{"exact", "unverified_content", "foreign_target", "extra_item", "foreign_before", "foreign_directory_evidence", "batch_mismatch", "batch_missing", "batch_nonhex", "batch_business_id"} {
			if strings.HasPrefix(mode, "batch_") && kind != "rename" {
				continue
			}
			if mode == "foreign_directory_evidence" && kind != "move" {
				continue
			}
			if kind == "mkdir" && (mode == "extra_item" || mode == "foreign_before") {
				continue
			}
			t.Run(kind+"/"+mode, func(t *testing.T) {
				parent, s := followIndependentStore(t)
				path, body, _ := followIndependentRequest(t, s, kind)
				switch mode {
				case "unverified_content":
					s.state.InitialContentVerified = false
				case "foreign_target":
					switch kind {
					case "mkdir":
						body["pdir_fid"] = "foreign-parent"
					case "move":
						body["to_pdir_fid"] = "foreign-parent"
					case "rename":
						body["rename_list"].([]map[string]string)[0]["fid"] = "foreign-file"
					}
				case "extra_item":
					if kind == "move" {
						body["fid_list"] = []string{s.state.Identity.SampleAgent, "foreign-file"}
					} else {
						body["rename_list"] = append(body["rename_list"].([]map[string]string), map[string]string{"fid": "foreign-file", "file_name": s.state.RenameName, "origin_file_name": s.state.Identity.Sample.Name})
					}
				case "foreign_before":
					id := s.state.RenameID
					if kind == "move" {
						id = s.state.MoveID
					}
					r := s.state.Operations[id]
					var p followOperation
					_ = json.Unmarshal(r.Payload, &p)
					p.Before.ID = "foreign-file"
					r.Payload, _ = json.Marshal(p)
					s.state.Operations[id] = r
				case "foreign_directory_evidence":
					r := s.state.Operations[s.state.DirectoryID]
					var p followOperation
					_ = json.Unmarshal(r.Payload, &p)
					p.Mapping.TV.ParentID = quark.QualifyID(quark.RouteTV, s.state.Identity.TVAccount, "foreign-root")
					r.Payload, _ = json.Marshal(p)
					s.state.Operations[s.state.DirectoryID] = r
				case "batch_mismatch":
					body["batch_id"] = "abcdef0123456789"
				case "batch_missing", "batch_nonhex", "batch_business_id":
					r := s.state.Operations[s.state.RenameID]
					var p followOperation
					_ = json.Unmarshal(r.Payload, &p)
					switch mode {
					case "batch_missing":
						p.BatchID = ""
					case "batch_nonhex":
						p.BatchID = "nothexnothexnope"
					case "batch_business_id":
						p.BatchID = s.state.RenameID
					}
					body["batch_id"] = p.BatchID
					r.Payload, _ = json.Marshal(p)
					s.state.Operations[r.ID] = r
				}
				g := newFollowGuard(s)
				g.delay = 0
				calls := 0
				w := g.wrap(observationOfflineTransport(func(*http.Request) (*http.Response, error) {
					calls++
					return observationOfflineResponse(`{"data":{}}`), nil
				}), false)
				resp, err := w.RoundTrip(followIndependentHTTP(path, body))
				if resp != nil {
					resp.Body.Close()
				}
				if mode == "exact" {
					if err != nil || calls != 1 {
						t.Fatal("exact receipt-owned write rejected")
					}
				} else if err == nil || calls != 0 || s.state.Requests != 0 {
					t.Fatal("unverified or foreign write dispatched")
				}
				if parent.unchanged() != nil {
					t.Fatal("follow-on changed completed parent journal/key")
				}
			})
		}
	}
}

func TestQuarkObservationFollowonIndependentUnknownWriteSurvivesReopen(t *testing.T) {
	for _, kind := range []string{"rename", "mkdir", "move"} {
		t.Run(kind, func(t *testing.T) {
			parent, s := followIndependentStore(t)
			path, body, key := followIndependentRequest(t, s, kind)
			started := s.state.Started
			calls := 0
			base := observationOfflineTransport(func(*http.Request) (*http.Response, error) {
				calls++
				encrypted, err := os.ReadFile(filepath.Join(s.dir, "followon.enc"))
				if err != nil {
					t.Fatal(err)
				}
				plain, err := s.cipher.Decrypt(followPurpose, string(encrypted))
				var durable followState
				if err != nil || json.Unmarshal([]byte(plain), &durable) != nil || !durable.Writes[key] || durable.Requests != 1 || durable.API != 1 {
					t.Fatal("write dispatched before durable intent/count")
				}
				return nil, errors.New("synthetic private upstream timeout")
			})
			g := newFollowGuard(s)
			g.delay = 0
			if _, err := g.wrap(base, false).RoundTrip(followIndependentHTTP(path, body)); err == nil || err.Error() != observationError().Error() {
				t.Fatal("unknown result leaked or succeeded")
			}
			s.lock.Close()
			reopened, err := openFollowStore(s.dir, parent)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.lock.Close()
			if reopened.state.Requests != 1 || reopened.state.API != 1 || !reopened.state.Started.Equal(started) || !reopened.state.Writes[key] {
				t.Fatal("reopen reset durable budget or unknown write")
			}
			g2 := newFollowGuard(reopened)
			g2.delay = 0
			if _, err = g2.wrap(base, false).RoundTrip(followIndependentHTTP(path, body)); err == nil || calls != 1 {
				t.Fatal("unknown write resubmitted after reopen")
			}
			if parent.unchanged() != nil {
				t.Fatal("completed parent journal changed")
			}
		})
	}
}

func TestQuarkObservationFollowonIndependentBudgetAndForbiddenOperations(t *testing.T) {
	parent, s := followIndependentStore(t)
	g := newFollowGuard(s)
	g.delay = 0
	calls := 0
	w := g.wrap(observationOfflineTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return observationOfflineResponse(`{"data":{}}`), nil
	}), false)
	for _, path := range []string{"/open/v1/file/upload_pre", "/open/v1/file/upload_finish", "/open/v1/file/delete", "/open/v1/file/copy"} {
		if _, err := w.RoundTrip(followIndependentHTTP(path, map[string]any{})); err == nil {
			t.Fatal("follow-on admitted extra operation")
		}
	}
	request := func() *http.Request {
		r, _ := http.NewRequestWithContext(context.Background(), "GET", "https://open-api-drive.quark.cn/user?method=user_info", nil)
		return r
	}
	for _, set := range []func(){func() { s.state.API = 200 }, func() { s.state.API = 0; s.state.Requests = 220 }, func() { s.state.Requests = 0; s.state.Started = time.Now().Add(-13 * time.Minute) }, func() { s.state.Started = time.Now(); s.state.Completed = true }} {
		set()
		if _, err := w.RoundTrip(request()); err == nil {
			t.Fatal("elapsed/completed/request budget bypass")
		}
	}
	if calls != 0 || parent.unchanged() != nil {
		t.Fatal("forbidden dispatch or parent mutation")
	}
}

func TestQuarkObservationFollowonIndependentParentIdentityEvidence(t *testing.T) {
	parent := followIndependentParent(t)
	for _, kind := range []string{"incomplete", "foreign_sample", "bad_mapping_revision", "bad_digest", "foreign_parent", "directory_proof"} {
		t.Run(kind, func(t *testing.T) {
			raw, _ := json.Marshal(parent.store.state)
			var st observationState
			_ = json.Unmarshal(raw, &st)
			if kind == "incomplete" {
				st.Completed = false
			} else {
				id := st.UploadID
				if kind == "directory_proof" {
					id = st.DirectoryID
				}
				r := st.Operations[id]
				var p map[string]json.RawMessage
				_ = json.Unmarshal(r.Payload, &p)
				if kind == "directory_proof" {
					var target followTarget
					_ = json.Unmarshal(p["target"], &target)
					target.Proof.Path = "/foreign"
					p["target"], _ = json.Marshal(target)
				} else {
					var m followMapping
					_ = json.Unmarshal(p["mapping"], &m)
					switch kind {
					case "foreign_sample":
						m.TV.ID = quark.QualifyID(quark.RouteTV, "foreign-account", "owned-sample")
					case "bad_mapping_revision":
						m.ReceiptRevision++
					case "bad_digest":
						m.SHA256 = "wrong"
					case "foreign_parent":
						m.TV.ParentID = quark.QualifyID(quark.RouteTV, st.TVAccount, "foreign-parent")
					}
					p["mapping"], _ = json.Marshal(m)
				}
				r.Payload, _ = json.Marshal(p)
				st.Operations[id] = r
			}
			if _, err := followIdentityFromState(st, []byte("synthetic-journal"), []byte("synthetic-key")); err == nil {
				t.Fatal("incomplete or mismatched parent ownership accepted")
			}
		})
	}
	if parent.unchanged() != nil {
		t.Fatal("identity validation changed parent")
	}
}

func TestQuarkObservationFollowonIndependentSameNamespaceMkdirEvidence(t *testing.T) {
	parent := followIndependentParent(t)
	for _, mode := range []string{"exact", "output_id", "output_parent", "output_name", "output_type", "foreign_account", "missing_output", "extra_output", "request_parent", "request_name", "proof", "target_path", "agent_parent", "tv_parent", "sample_tv_parent"} {
		t.Run(mode, func(t *testing.T) {
			raw, _ := json.Marshal(parent.store.state)
			var st observationState
			if json.Unmarshal(raw, &st) != nil {
				t.Fatal("synthetic state clone")
			}
			r := st.Operations[st.DirectoryID]
			var p followOperation
			if json.Unmarshal(r.Payload, &p) != nil {
				t.Fatal("synthetic directory clone")
			}
			p.Mapping = nil
			p.Handle = "root-listed"
			p.Request = cloud.OperationRequest{ID: r.ID, Kind: "mkdir", ParentID: quark.QualifyID(quark.RouteAgent, st.AgentAccount, "proof-listed"), Name: st.DirectoryName}
			p.Outputs = []cloud.Item{{ID: quark.QualifyID(quark.RouteAgent, st.AgentAccount, p.Handle), ParentID: p.Request.ParentID, Name: st.DirectoryName, IsDir: true}}
			switch mode {
			case "output_id":
				p.Outputs[0].ID = quark.QualifyID(quark.RouteAgent, st.AgentAccount, "unacknowledged-directory")
			case "output_parent":
				p.Outputs[0].ParentID = quark.QualifyID(quark.RouteAgent, st.AgentAccount, "foreign-parent")
			case "output_name":
				p.Outputs[0].Name = "unowned-directory"
			case "output_type":
				p.Outputs[0].IsDir = false
			case "foreign_account":
				p.Outputs[0].ID = quark.QualifyID(quark.RouteAgent, "foreign-account", p.Handle)
			case "missing_output":
				p.Outputs = nil
			case "extra_output":
				p.Outputs = append(p.Outputs, p.Outputs[0])
			case "request_parent":
				p.Request.ParentID = quark.QualifyID(quark.RouteAgent, st.AgentAccount, "foreign-parent")
			case "request_name":
				p.Request.Name = "unowned-name"
			case "proof":
				p.Target.Proof.Path = "/foreign"
			case "target_path":
				p.Target.Path = "/foreign"
			case "agent_parent":
				p.Target.AgentParent = "foreign-parent"
			case "tv_parent":
				p.Target.TVParent = quark.QualifyID(quark.RouteTV, st.TVAccount, "foreign-parent")
			case "sample_tv_parent":
				u := st.Operations[st.UploadID]
				var upload map[string]json.RawMessage
				_ = json.Unmarshal(u.Payload, &upload)
				var target followTarget
				_ = json.Unmarshal(upload["target"], &target)
				target.TVParent = quark.QualifyID(quark.RouteTV, st.TVAccount, "foreign-parent")
				upload["target"], _ = json.Marshal(target)
				u.Payload, _ = json.Marshal(upload)
				st.Operations[st.UploadID] = u
			}
			r.Payload, _ = json.Marshal(p)
			st.Operations[st.DirectoryID] = r
			identity, err := followIdentityFromState(st, []byte("synthetic-journal"), []byte("synthetic-key"))
			if mode == "exact" {
				if err != nil {
					t.Fatalf("native same-namespace confirmed output rejected: %v", err)
				}
				if identity.Root.ID != parent.identity.Root.ID || identity.Sample.ID != parent.identity.Sample.ID || identity.RootAgent != parent.identity.RootAgent || identity.RootACK != p.Handle {
					t.Fatal("same-namespace path broadened original ownership")
				}
			} else if err == nil {
				t.Fatal("mismatched same-namespace directory evidence accepted")
			}
		})
	}
	if parent.unchanged() != nil {
		t.Fatal("identity compatibility changed original journal/key")
	}
}
