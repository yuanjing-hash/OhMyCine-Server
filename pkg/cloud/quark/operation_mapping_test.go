package quark

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// These fake domains reproduce the observed provider behavior without using
// real handles: ACK is a full opaque ID, listing has a fingerprint suffix.
type createdMappingFixture struct {
	d             *Client
	store         *memoryOperations
	created       bool
	directory     bool
	name          string
	listed        string
	root          string
	tv            string
	revision      string
	content       []byte
	duplicate     bool
	wrongParent   bool
	wrongScope    bool
	writes, reads int
}

func newCreatedMappingFixture(t *testing.T, directory bool) *createdMappingFixture {
	t.Helper()
	f := &createdMappingFixture{directory: directory, name: "created.bin", listed: "opaque-list|created", root: "opaque-list|root", tv: "tv-created", content: []byte("abc")}
	f.d, f.store = managedFixture(t, func(r *http.Request) (*http.Response, error) {
		size := int64(len(f.content))
		if directory {
			size = 0
		}
		switch r.URL.Path {
		case "/open/v1/file/info":
			id := r.URL.Query().Get("fid")
			if sameAgent(id, "opaque-list|root") || id == "ack-root" {
				return envelope(r, agentFile(id, "0", "library", true, 0)), nil
			}
			parent := "ack-root"
			if f.wrongParent {
				parent = "other-ack-parent"
			}
			return envelope(r, agentFile(id, parent, f.name, directory, size)), nil
		case "/open/v1/file/list":
			var in struct {
				Parent string `json:"parent_fid"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			files := []any{}
			if in.Parent == "0" {
				files = append(files, agentFile(f.root, "0", "library", true, 0))
			} else if sameAgent(in.Parent, "opaque-list|root") && f.created {
				files = append(files, agentFile(f.listed, in.Parent, f.name, directory, size))
				if f.duplicate {
					files = append(files, agentFile("other-list|duplicate", in.Parent, f.name, directory, size))
				}
			} else if !sameAgent(in.Parent, "opaque-list|root") {
				t.Fatal("unrelated directory traversed")
			}
			return envelope(r, map[string]any{"last_page": true, "file_list": files}), nil
		case "/open/v1/file/upload_pre":
			f.writes++
			f.created = true
			return envelope(r, map[string]any{"fid": "creation-ack", "finish": true}), nil
		case "/open/v1/dir":
			f.writes++
			f.created = true
			return envelope(r, map[string]any{"fid": "creation-ack"}), nil
		case "/file":
			if r.URL.Query().Get("method") == "download" {
				if r.URL.Query().Get("fid") != f.tv {
					t.Fatal("non-TV handle used for download")
				}
				return envelope(r, map[string]any{"fid": f.tv, "file_name": f.name, "size": size, "download_url": "https://objects.example/created"}), nil
			}
			files := []any{}
			if r.URL.Query().Get("parent_fid") == "0" {
				id := "T"
				if f.wrongScope {
					id = "replaced-root"
				}
				files = append(files, map[string]any{"fid": id, "filename": "library", "size": 0, "isdir": 1})
			} else if r.URL.Query().Get("parent_fid") == "T" && f.created {
				kind := 0
				if directory {
					kind = 1
				}
				files = append(files, map[string]any{"fid": f.tv, "filename": f.name, "size": size, "isdir": kind, "version": f.revision})
			} else {
				t.Fatal("unexpected TV parent")
			}
			return envelope(r, map[string]any{"total_count": len(files), "files": files}), nil
		}
		t.Fatalf("unexpected API %s", r.URL.Path)
		return nil, nil
	})
	f.d.credentials.Proofs[0].AgentRoot = QualifyID(RouteAgent, "agent-account", "opaque-list|root")
	f.d.mediaTransport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Fatal("readback not anonymous")
		}
		f.reads++
		return &http.Response{StatusCode: 200, ContentLength: int64(len(f.content)), Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(f.content))}, nil
	})
	return f
}
func (f *createdMappingFixture) create(ctx context.Context) (cloud.Item, error) {
	parent := QualifyID(RouteAgent, "agent-account", f.root)
	if f.directory {
		return f.d.CreateDirectory(ctx, parent, f.name)
	}
	return f.d.Upload(ctx, cloud.UploadRequest{ParentID: parent, Name: f.name, Size: 3, Reader: bytes.NewReader([]byte("abc"))})
}
func (f *createdMappingFixture) restart(t *testing.T) {
	t.Helper()
	raw, _ := json.Marshal(f.d.Credentials())
	d, e := NewClient(cloud.Config{Credentials: raw, Operations: f.store})
	if e != nil {
		t.Fatal(e)
	}
	d.api.http.Transport = f.d.api.http.Transport
	d.mediaTransport = f.d.mediaTransport
	f.d = d
}

func TestCreatedCrossDomainMappingPersistsAndPinsOutputAcrossRestart(t *testing.T) {
	for _, dir := range []bool{false, true} {
		t.Run(map[bool]string{false: "upload", true: "mkdir"}[dir], func(t *testing.T) {
			f := newCreatedMappingFixture(t, dir)
			ctx := WithOperationID(context.Background(), "operation-one")
			out, e := f.create(ctx)
			if e != nil {
				t.Fatalf("create: %v", e)
			}
			if f.writes != 1 {
				t.Fatal("submission count")
			}
			if sameAgent("creation-ack", f.listed) {
				t.Fatal("test did not exercise independent ID domains")
			}
			f.restart(t)
			f.root = "new-read-prefix|root"
			f.listed = "new-read-prefix|created"
			again, e := f.create(ctx)
			if e != nil || again.ID != out.ID || f.writes != 1 {
				t.Fatalf("restart replayed upload or lost ACK: %v", e)
			}
			mapped, e := f.d.ResolveReadItem(ctx, out.ID)
			if e != nil || mapped.ID != QualifyID(RouteTV, "tv-account", f.tv) || mapped.ParentID != QualifyID(RouteTV, "tv-account", "T") {
				t.Fatalf("receipt mapping failed: %v", e)
			}
			if !dir && f.reads != 2 {
				t.Fatalf("initial and resumed mapping lacked complete digest readbacks: %d", f.reads)
			}
			_, e = f.d.ResolveReadItem(WithOperationID(context.Background(), "wrong-operation"), out.ID)
			if e == nil {
				t.Fatal("wrong receipt mapped")
			}
			_, e = f.d.ResolveReadItem(context.Background(), out.ID)
			if e == nil {
				t.Fatal("arbitrary cross-domain ACK mapped without receipt")
			}
			f.tv = "same-path-replacement"
			_, e = f.d.ResolveReadItem(ctx, out.ID)
			errorCode(t, e, CodeIdentity)
			if f.writes != 1 {
				t.Fatal("mapping resubmitted original operation")
			}
		})
	}
}

func TestQualifiedAgentIdentityKeepsRouteAccountAndFingerprint(t *testing.T) {
	q := QualifyID(RouteAgent, "account", "initial|root")
	if !qualifiedAgentMatches(q, "account", "renewed|root") {
		t.Fatal("same fingerprint rejected")
	}
	for _, v := range []struct{ q, a, id string }{{q, "other-account", "renewed|root"}, {q, "account", "renewed|other"}, {q, "account", "root"}, {QualifyID(RouteTV, "account", "initial|root"), "account", "initial|root"}} {
		if qualifiedAgentMatches(v.q, v.a, v.id) {
			t.Fatal("namespace, account or fingerprint mismatch accepted")
		}
	}
}

func TestCrossDomainCreationRejectsMissingProofAndContradictoryEvidence(t *testing.T) {
	for _, mode := range []string{"wrong_content", "duplicate", "wrong_parent", "replaced_scope", "same_domain_other_id", "list_replaced", "proof_changed", "wrong_receipt_account"} {
		t.Run(mode, func(t *testing.T) {
			f := newCreatedMappingFixture(t, false)
			ctx := WithOperationID(context.Background(), "op")
			switch mode {
			case "wrong_content":
				f.content = []byte("xyz")
			case "duplicate":
				f.duplicate = true
			case "wrong_parent":
				f.wrongParent = true
			case "replaced_scope":
				f.wrongScope = true
			case "same_domain_other_id":
				f.listed = "another-ack"
			}
			out, e := f.create(ctx)
			if mode == "list_replaced" || mode == "proof_changed" || mode == "wrong_receipt_account" {
				if e != nil {
					t.Fatal(e)
				}
				f.restart(t)
				switch mode {
				case "list_replaced":
					f.listed = "replaced|different-fingerprint"
				case "proof_changed":
					f.d.credentials.Proofs[0].Generation++
				case "wrong_receipt_account":
					r := f.store.receipts["op"]
					r.AccountID = "different-account"
					f.store.receipts["op"] = r
				}
				_, e = f.d.ResolveReadItem(ctx, out.ID)
			}
			if e == nil {
				t.Fatal("contradictory evidence accepted")
			}
			if mode == "wrong_content" && f.store.receipts["op"].State == "confirmed" {
				t.Fatal("mismatching content published")
			}
			if f.writes != 1 {
				t.Fatal("unexpected second write")
			}
		})
	}
}

func TestCrossDomainMappingCannotBeSynthesizedFromAnUnrelatedReceipt(t *testing.T) {
	f := newCreatedMappingFixture(t, false)
	ctx := WithOperationID(context.Background(), "op")
	_, e := f.create(ctx)
	if e != nil {
		t.Fatal(e)
	}
	_, e = f.d.ResolveReadItem(ctx, QualifyID(RouteAgent, "agent-account", "unrelated-ack"))
	errorCode(t, e, CodeIdentity)
	r := f.store.receipts["op"]
	var p uploadPayload
	_ = json.Unmarshal(r.Payload, &p)
	p.Mapping.SHA256 = strings.Repeat("0", 64)
	r.Payload, _ = json.Marshal(p)
	f.store.receipts["op"] = r
	_, e = f.d.ResolveReadItem(ctx, p.Output.ID)
	errorCode(t, e, CodeIdentity)
}

func TestCrossDomainUploadReadbackFailureResumesSameAcknowledgement(t *testing.T) {
	f := newCreatedMappingFixture(t, false)
	f.content = []byte("xyz")
	ctx := WithOperationID(context.Background(), "recover-created-output")
	_, err := f.create(ctx)
	errorCode(t, err, CodeIdentity)
	r := f.store.receipts["recover-created-output"]
	var p uploadPayload
	if json.Unmarshal(r.Payload, &p) != nil || r.State != "accepted" || p.Handle != "creation-ack" || p.Target == nil || p.Mapping != nil || f.writes != 1 {
		t.Fatal("unconfirmed mapping lost original write receipt")
	}
	f.restart(t)
	f.content = []byte("abc")
	out, err := f.create(ctx)
	if err != nil || f.writes != 1 {
		t.Fatalf("recovery resubmitted a creation: %v", err)
	}
	if _, err = f.d.ResolveReadItem(ctx, out.ID); err != nil {
		t.Fatal(err)
	}
	if f.store.receipts["recover-created-output"].State != "confirmed" {
		t.Fatal("verified mapping not persisted")
	}
}

func TestUploadReadbackCandidateDoesNotRepeatContentOrWeakenColdResolver(t *testing.T) {
	f := newCreatedMappingFixture(t, false)
	ctx := WithOperationID(context.Background(), "upload-candidate")
	out, err := f.create(ctx)
	if err != nil || f.reads != 1 {
		t.Fatal("native creation mapping not fully verified", err)
	}
	before := f.store.receipts["upload-candidate"]
	f.restart(t)
	f.root, f.listed = "renewed|root", "renewed|created"
	item, err := f.d.ResolveUploadReadbackItem(ctx, out.ID)
	if err != nil || item.ID != QualifyID(RouteTV, "tv-account", f.tv) || f.reads != 1 || f.writes != 1 {
		t.Fatalf("candidate repeated content or changed identity: %v reads=%d writes=%d", err, f.reads, f.writes)
	}
	after := f.store.receipts["upload-candidate"]
	if before.Revision != after.Revision || before.State != after.State || !bytes.Equal(before.Payload, after.Payload) {
		t.Fatal("metadata candidate changed the confirmed private receipt")
	}
	// A candidate is deliberately not proof that these bytes still match.
	// The generic resolver must still detect invisible same-size replacement.
	f.content = []byte("xyz")
	if _, err = f.d.ResolveUploadReadbackItem(ctx, out.ID); err != nil || f.reads != 1 {
		t.Fatal("candidate unexpectedly downloaded or verified content", err)
	}
	_, err = f.d.ResolveReadItem(ctx, out.ID)
	errorCode(t, err, CodeIdentity)
	if f.reads != 2 || f.writes != 1 {
		t.Fatal("cold strict resolution skipped content or repeated upload")
	}
}

func TestUploadReadbackCandidateRejectsUnboundOrChangedEvidence(t *testing.T) {
	for _, mode := range []string{"missing_operation", "wrong_operation", "wrong_ack", "accepted", "phase", "receipt_id", "receipt_revision", "account", "digest", "mapping_digest", "output_size", "output_name", "output_directory", "output_parent", "list", "duplicate", "parent", "root", "tv", "tv_revision", "proof", "challenge"} {
		t.Run(mode, func(t *testing.T) {
			f := newCreatedMappingFixture(t, false)
			ctx := WithOperationID(context.Background(), "upload-candidate")
			out, err := f.create(ctx)
			if err != nil {
				t.Fatal(err)
			}
			f.restart(t)
			r := f.store.receipts["upload-candidate"]
			var p uploadPayload
			if err = json.Unmarshal(r.Payload, &p); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "missing_operation":
				ctx = context.Background()
			case "wrong_operation":
				ctx = WithOperationID(context.Background(), "unrelated")
			case "wrong_ack":
				out.ID = QualifyID(RouteAgent, "agent-account", "unrelated-ack")
			case "accepted":
				r.State = "accepted"
			case "phase":
				p.Phase = "finish_received"
			case "receipt_id":
				r.ID = "unrelated"
			case "receipt_revision":
				r.Revision++
			case "account":
				r.AccountID = "other-account"
			case "digest":
				p.SHA256 = strings.Repeat("f", 64)
			case "mapping_digest":
				p.Mapping.SHA256 = strings.Repeat("f", 64)
			case "output_size":
				p.Output.Size++
			case "output_name":
				p.Output.Name = "other.bin"
			case "output_directory":
				p.Output.IsDir = true
			case "output_parent":
				p.Output.ParentID = QualifyID(RouteAgent, "agent-account", "other-ack-root")
			case "list":
				f.listed = "other|replacement"
			case "duplicate":
				f.duplicate = true
			case "parent":
				f.wrongParent = true
			case "root":
				f.wrongScope = true
			case "tv":
				f.tv = "replacement"
			case "tv_revision":
				f.revision = "changed"
			case "proof":
				f.d.credentials.Proofs[0].Generation++
			case "challenge":
				p.Target.Challenge = true
			}
			r.Payload, _ = json.Marshal(p)
			f.store.receipts["upload-candidate"] = r
			if _, err = f.d.ResolveUploadReadbackItem(ctx, out.ID); err == nil {
				t.Fatal("unbound or changed evidence authorized a candidate")
			}
			if f.reads != 1 || f.writes != 1 {
				t.Fatal("rejection performed a media read or repeated a mutation")
			}
		})
	}
}
