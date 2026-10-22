package quark

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

func TestManagementMappingPinsObservedTVIdentityAcrossFreshPathLookup(t *testing.T) {
	for _, replaced := range []bool{false, true} {
		t.Run(map[bool]string{false: "same_item", true: "same_path_replacement"}[replaced], func(t *testing.T) {
			calls := 0
			d, _ := managedFixture(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path == "/file" {
					parent := r.URL.Query().Get("parent_fid")
					files := []any{}
					if parent == "0" {
						files = append(files, map[string]any{"fid": "T", "parent_fid": "0", "filename": "library", "isdir": 1, "size": 0})
					} else if parent == "T" {
						id := "tv-original"
						if replaced {
							id = "tv-replacement"
						}
						files = append(files, map[string]any{"fid": id, "parent_fid": "T", "filename": "Episode.mkv", "isdir": 0, "size": 9})
					} else {
						t.Fatalf("mapping traversed unrelated directory: %s", parent)
					}
					return envelope(r, map[string]any{"total_count": len(files), "files": files}), nil
				}
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				file := agentFile("A", "0", "library", true, 0)
				if body["parent_fid"] == "A" {
					file = agentFile("agent-file", "A", "Episode.mkv", false, 9)
				}
				return envelope(r, map[string]any{"last_page": true, "file_list": []any{file}}), nil
			})
			id := QualifyID(RouteTV, "tv-account", "tv-original")
			d.observed[id] = cloud.Item{ID: id, ParentID: QualifyID(RouteTV, "tv-account", "T"), Name: "Episode.mkv", Size: 9}
			item, err := d.ResolveManagementItem(context.Background(), id)
			if replaced {
				errorCode(t, err, CodeIdentity)
			} else if err != nil || item.ID != QualifyID(RouteAgent, "agent-account", "agent-file") {
				t.Fatalf("mapping failed: %v", err)
			}
			if calls > 6 {
				t.Fatalf("observed identity used subtree search instead of bounded path: %d calls", calls)
			}
		})
	}
}
