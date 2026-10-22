//go:build quark_live

package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

// Exercise the actual driver and handler actor through the same guard. All
// transport calls terminate in this in-memory provider, never in a socket.
func TestQuarkObservationFollowonNativeActor(t *testing.T) {
	t.Run("cross_namespace_mkdir", func(t *testing.T) { followNativeActorFixture(t, false) })
	t.Run("same_namespace_mkdir", func(t *testing.T) { followNativeActorFixture(t, true) })
}

func followNativeActorFixture(t *testing.T, sameNamespace bool) {
	_, s := followIndependentStore(t)
	i := &s.state.Identity
	i.RootAgent, i.SampleAgent, i.ProofAgent = "list|root", "list|sample", "list|proof"
	i.Proof.AgentRoot = quark.QualifyID(quark.RouteAgent, i.AgentAccount, i.ProofAgent)
	s.state.Bundle.Proofs = []quark.SpaceProof{i.Proof}
	if e := s.save(); e != nil {
		t.Fatal(e)
	}
	g := newFollowGuard(s)
	g.delay = 0
	g.startup = false
	fixture, e := os.ReadFile(filepath.Join("..", "..", "..", "validation", "observation-fixture", "omc-observation-fixture.mp4"))
	if e != nil || len(fixture) != observationFixtureSize || followDigest(fixture) != observationFixtureSHA {
		t.Fatal("synthetic fixture unavailable")
	}
	tvRoot, _ := followRawID(i.Root.ID, quark.RouteTV, i.TVAccount)
	tvProof, _ := followRawID(i.Proof.TVRoot, quark.RouteTV, i.TVAccount)
	tvSample, _ := followRawID(i.Sample.ID, quark.RouteTV, i.TVAccount)
	name, parent, newDir := i.Sample.Name, i.RootAgent, false
	newACK := "new-ack"
	if sameNamespace {
		newACK = "list|new"
	}
	writes := map[string]int{}
	info := func(id, par, n string, dir bool, size int64) map[string]any {
		kind := "1"
		if dir {
			kind = "0"
		}
		return map[string]any{"fid": id, "parent_fid": par, "filename": n, "file_type": kind, "size": size}
	}
	tvInfo := func(id, par, n string, dir bool, size int64) map[string]any {
		kind := 0
		if dir {
			kind = 1
		}
		return map[string]any{"fid": id, "parent_fid": par, "filename": n, "isdir": kind, "size": size}
	}
	fixtureTransport := observationOfflineTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "fixture.invalid" {
			return observationOfflineResponse(string(fixture)), nil
		}
		var in map[string]json.RawMessage
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&in)
		}
		str := func(k string) string { var x string; _ = json.Unmarshal(in[k], &x); return x }
		var data any
		switch r.URL.Path {
		case "/open/v1/file/info":
			switch r.URL.Query().Get("fid") {
			case i.SampleAgent:
				data = info(i.SampleAgent, parent, name, false, observationFixtureSize)
			case i.RootAgent:
				data = info(i.RootAgent, i.ProofAgent, i.Root.Name, true, 0)
			case i.RootACK:
				data = info(i.RootACK, i.ProofRootACK, i.Root.Name, true, 0)
			case i.ProofAgent:
				data = info(i.ProofAgent, "0", s.state.Identity.Proof.Path[1:], true, 0)
			case "new-ack":
				data = info("new-ack", i.RootACK, s.state.DirectoryName, true, 0)
			case "list|new":
				data = info("list|new", i.RootAgent, s.state.DirectoryName, true, 0)
			default:
				t.Fatal("unrecognized synthetic info identity")
			}
		case "/open/v1/file/list":
			files := []map[string]any{}
			switch str("parent_fid") {
			case "0":
				files = append(files, info(i.ProofAgent, "0", i.Proof.Path[1:], true, 0))
			case i.ProofAgent:
				files = append(files, info(i.RootAgent, i.ProofAgent, i.Root.Name, true, 0))
			case i.RootAgent:
				if parent == i.RootAgent {
					files = append(files, info(i.SampleAgent, parent, name, false, observationFixtureSize))
				}
				if newDir {
					files = append(files, info("list|new", i.RootAgent, s.state.DirectoryName, true, 0))
				}
			case "list|new":
				if parent == "list|new" {
					files = append(files, info(i.SampleAgent, parent, name, false, observationFixtureSize))
				}
			default:
				t.Fatal("unrecognized synthetic directory")
			}
			data = map[string]any{"file_list": files, "last_page": true}
		case "/file":
			if r.URL.Query().Get("method") == "download" {
				data = map[string]any{"fid": tvSample, "file_name": name, "size": observationFixtureSize, "download_url": "https://fixture.invalid/owned-sample"}
				break
			}
			files := []map[string]any{}
			switch r.URL.Query().Get("parent_fid") {
			case "0":
				files = append(files, tvInfo(tvProof, "0", i.Proof.Path[1:], true, 0))
			case tvProof:
				files = append(files, tvInfo(tvRoot, tvProof, i.Root.Name, true, 0))
			case tvRoot:
				if parent == i.RootAgent {
					files = append(files, tvInfo(tvSample, tvRoot, name, false, observationFixtureSize))
				}
				if newDir {
					files = append(files, tvInfo("tv-new", tvRoot, s.state.DirectoryName, true, 0))
				}
			case "tv-new":
				if parent == "list|new" {
					files = append(files, tvInfo(tvSample, "tv-new", name, false, observationFixtureSize))
				}
			default:
				t.Fatal("unrecognized synthetic TV directory")
			}
			data = map[string]any{"files": files, "total_count": len(files)}
		case "/open/v1/file/rename/batch":
			writes["rename"]++
			name = s.state.RenameName
			data = map[string]any{"finish": true}
		case "/open/v1/dir":
			writes["mkdir"]++
			newDir = true
			data = map[string]any{"fid": newACK, "finish": true}
		case "/open/v1/file/move":
			writes["move"]++
			parent = "list|new"
			data = map[string]any{"finish": true}
		default:
			t.Fatal("unrecognized synthetic endpoint")
		}
		b, _ := json.Marshal(map[string]any{"status": 0, "data": data})
		return observationOfflineResponse(string(b)), nil
	})
	raw, e := s.credentials(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	d, e := quark.NewObservationValidationClient(cloud.Config{Credentials: raw, Operations: s}, func(_ http.RoundTripper, media bool) http.RoundTripper { return g.wrap(fixtureTransport, media) })
	if e != nil {
		t.Fatal(e)
	}
	for _, stage := range []string{"rename_one", "mkdir_move_one"} {
		if e = followMutate(context.Background(), d, s, stage); e != nil {
			t.Fatalf("stage=%s code=%v endpoint=%s outcome=%s", stage, e, g.lastEndpoint, g.lastOutcome)
		}
	}
	if writes["rename"] != 1 || writes["mkdir"] != 1 || writes["move"] != 1 || len(writes) != 3 || s.state.Reads != 1 || s.state.ReadBytes != followReadLimit {
		t.Fatal("native mutation or content accounting mismatch")
	}
	var created followOperation
	if json.Unmarshal(s.state.Operations[s.state.DirectoryID].Payload, &created) != nil || (created.Mapping == nil) != sameNamespace {
		t.Fatal("native mkdir receipt shape mismatch")
	}
	// A cold native client can confirm the same completed actor without another
	// cloud mutation. It may independently read the owned directory/content.
	d, e = quark.NewObservationValidationClient(cloud.Config{Credentials: raw, Operations: s}, func(_ http.RoundTripper, media bool) http.RoundTripper { return g.wrap(fixtureTransport, media) })
	if e != nil {
		t.Fatal(e)
	}
	if e = followMutate(context.Background(), d, s, "mkdir_move_one"); e != nil {
		t.Fatalf("cold actor: %v endpoint=%s outcome=%s", e, g.lastEndpoint, g.lastOutcome)
	}
	if writes["rename"] != 1 || writes["mkdir"] != 1 || writes["move"] != 1 {
		t.Fatal("cold actor repeated cloud write")
	}
	t.Logf("synthetic_native_total_requests=%d api=%d media=%d writes=3", s.state.Requests, s.state.API, s.state.Reads)
}
