package quark

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

func TestLostRenameAndMoveAcknowledgementRecoverWithoutResubmission(t *testing.T) {
	for _, kind := range []string{"rename", "move"} {
		t.Run(kind, func(t *testing.T) {
			mutated, writes := false, 0
			file := func() map[string]any {
				parent, name := "A", "old.mkv"
				if mutated {
					if kind == "rename" {
						name = "new.mkv"
					} else {
						parent = "B"
					}
				}
				return agentFile("f", parent, name, false, 9)
			}
			d, store := managedFixture(t, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/open/v1/file/info":
					switch r.URL.Query().Get("fid") {
					case "A":
						return envelope(r, agentFile("A", "0", "library", true, 0)), nil
					case "B":
						return envelope(r, agentFile("B", "A", "destination", true, 0)), nil
					default:
						return envelope(r, file()), nil
					}
				case "/open/v1/file/list":
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					files := []any{}
					if body["parent_fid"] == file()["parent_fid"] {
						files = append(files, file())
					}
					return envelope(r, map[string]any{"last_page": true, "file_list": files}), nil
				case "/open/v1/file/rename/batch", "/open/v1/file/move":
					writes++
					mutated = true
					return nil, errors.New("lost acknowledgement")
				default:
					t.Fatalf("unexpected recovery route: %s", r.URL.Path)
					return nil, nil
				}
			})
			req := cloud.OperationRequest{ID: "lost-ack", Kind: kind, ItemID: QualifyID(RouteAgent, "agent-account", "f")}
			if kind == "rename" {
				req.Name = "new.mkv"
			} else {
				req.ParentID = QualifyID(RouteAgent, "agent-account", "B")
			}
			_, err := d.BeginOperation(context.Background(), req)
			errorCode(t, err, cloud.CodeMutationUnknown)
			// A fresh instance has no in-memory observation evidence.
			raw, _ := json.Marshal(d.Credentials())
			restarted, err := NewClient(cloud.Config{Credentials: raw, Operations: store})
			if err != nil {
				t.Fatal(err)
			}
			restarted.api.http.Transport = d.api.http.Transport
			result, err := restarted.BeginOperation(context.Background(), req)
			if err != nil || !result.Complete || result.Receipt.State != "confirmed" || writes != 1 || len(result.Items) != 1 {
				t.Fatalf("lost acknowledgement did not reconcile: complete=%v state=%s writes=%d err=%v", result.Complete, result.Receipt.State, writes, err)
			}
		})
	}
}
