//go:build quark_live

package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

func TestQuarkObservationRetirementNativeActor(t *testing.T) {
	for _, mode := range []string{"normal", "lost_move_ack", "target_conflict"} {
		t.Run(mode, func(t *testing.T) {
			creation, parent, s := retirementIndependentStore(t)
			i, c := s.state.Identity, s.state.Identity.Creation
			g := newRetirementGuard(s)
			g.delay = 0
			g.startup = false
			agentParent := i.InsideAgent
			tvParent := i.Inside.ID
			writes := 0
			outsideReads, sourceReads := 0, 0
			rawTV := func(q string) string {
				id, ok := followRawID(q, quark.RouteTV, c.TVAccount)
				if !ok {
					t.Fatal("synthetic namespace")
				}
				return id
			}
			info := func(id, parent, name string, dir bool, size int64) map[string]any {
				kind := "1"
				if dir {
					kind = "0"
				}
				return map[string]any{"fid": id, "parent_fid": parent, "filename": name, "file_type": kind, "size": size}
			}
			tv := func(id, parent, name string, dir bool, size int64) map[string]any {
				kind := 0
				if dir {
					kind = 1
				}
				return map[string]any{"fid": id, "parent_fid": parent, "filename": name, "isdir": kind, "size": size}
			}
			base := observationOfflineTransport(func(r *http.Request) (*http.Response, error) {
				var in map[string]json.RawMessage
				if r.Body != nil {
					_ = json.NewDecoder(r.Body).Decode(&in)
				}
				str := func(k string) string { var x string; _ = json.Unmarshal(in[k], &x); return x }
				var data any
				switch r.URL.Path {
				case "/open/v1/file/info":
					switch id := r.URL.Query().Get("fid"); id {
					case c.SampleAgent:
						data = info(id, agentParent, i.Name, false, observationFixtureSize)
					case c.RootAgent:
						data = info(id, c.ProofAgent, c.Root.Name, true, 0)
					case c.ProofAgent:
						data = info(id, "0", strings.TrimPrefix(c.Proof.Path, "/"), true, 0)
					case i.InsideAgent:
						data = info(id, c.RootAgent, i.DirectoryName, true, 0)
					default:
						t.Fatal("unapproved info")
					}
				case "/open/v1/file/list":
					p := str("parent_fid")
					files := []map[string]any{}
					switch p {
					case c.ProofAgent:
						files = append(files, info(c.RootAgent, p, c.Root.Name, true, 0))
					case c.RootAgent:
						files = append(files, info(i.InsideAgent, p, i.DirectoryName, true, 0))
					case i.InsideAgent:
					default:
						t.Fatal("unapproved list")
					}
					if p == agentParent {
						files = append(files, info(c.SampleAgent, p, i.Name, false, observationFixtureSize))
					}
					if mode == "target_conflict" && p == c.ProofAgent {
						files = append(files, info("foreign-sample", p, i.Name, false, observationFixtureSize))
					}
					data = map[string]any{"file_list": files, "last_page": true}
				case "/file":
					if r.URL.Query().Get("method") != "list" {
						t.Fatal("media request")
					}
					p := r.URL.Query().Get("parent_fid")
					files := []map[string]any{}
					switch p {
					case rawTV(c.Proof.TVRoot):
						outsideReads++
						files = append(files, tv(rawTV(c.Root.ID), p, c.Root.Name, true, 0))
					case rawTV(c.Root.ID):
						files = append(files, tv(rawTV(i.Inside.ID), p, i.DirectoryName, true, 0))
					case rawTV(i.Inside.ID):
						sourceReads++
					default:
						t.Fatal("unapproved TV list")
					}
					if p == rawTV(tvParent) {
						files = append(files, tv(rawTV(c.Sample.ID), p, i.Name, false, observationFixtureSize))
					}
					if mode == "target_conflict" && p == rawTV(c.Proof.TVRoot) {
						files = append(files, tv("foreign-sample", p, i.Name, false, observationFixtureSize))
					}
					data = map[string]any{"files": files, "total_count": len(files)}
				case "/open/v1/file/move":
					writes++
					agentParent = str("to_pdir_fid")
					if agentParent == c.ProofAgent {
						tvParent = c.Proof.TVRoot
					} else if agentParent == i.InsideAgent {
						tvParent = i.Inside.ID
					} else {
						t.Fatal("unapproved move target")
					}
					if mode == "lost_move_ack" && writes == 1 {
						return nil, errors.New("synthetic unknown transport response")
					}
					data = map[string]any{"finish": true}
				default:
					t.Fatal("unapproved mutation or endpoint")
				}
				body, _ := json.Marshal(map[string]any{"status": 0, "data": data})
				return observationOfflineResponse(string(body)), nil
			})
			raw, _ := s.credentials(context.Background())
			client := func() *quark.Client {
				d, e := quark.NewObservationValidationClient(cloud.Config{Credentials: raw, Operations: s}, func(_ http.RoundTripper, media bool) http.RoundTripper { return g.wrap(base, media) })
				if e != nil {
					t.Fatal(e)
				}
				return d
			}
			d := client()
			err := retirementMutate(context.Background(), d, s, "move_out")
			if mode == "target_conflict" {
				if err == nil || outsideReads < 2 || sourceReads < 1 || writes != 0 || len(s.state.Operations) != 0 || len(s.state.Preflights) != 0 {
					t.Fatal("conflict admitted")
				}
				return
			}
			if mode == "lost_move_ack" {
				if err == nil || s.state.Operations[s.state.MoveOutID].State != "unknown" {
					t.Fatal("lost ACK not retained")
				}
				d = client()
				err = retirementMutate(context.Background(), d, s, "move_out")
			}
			if err != nil {
				t.Fatalf("move out: %v endpoint=%s", err, g.lastEndpoint)
			}
			if writes != 1 {
				t.Fatal("move resubmitted")
			}
			d = client()
			if err = retirementMutate(context.Background(), d, s, "move_back"); err != nil {
				t.Fatalf("move back: %v endpoint=%s", err, g.lastEndpoint)
			}
			if writes != 2 || agentParent != i.InsideAgent || tvParent != i.Inside.ID || len(s.state.Writes) != 2 || creation.unchanged() != nil || parent.unchanged() != nil {
				t.Fatal("frozen actors or predecessor preservation failed")
			}
			d = client()
			if err = retirementMutate(context.Background(), d, s, "move_back"); err != nil || writes != 2 {
				t.Fatal("confirmed actor replay wrote cloud")
			}
		})
	}
}
