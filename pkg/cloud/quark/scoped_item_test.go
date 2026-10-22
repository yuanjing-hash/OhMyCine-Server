package quark

import (
	"context"
	"net/http"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

func TestScopedPlaybackResolvesColdDriverWithinRoot(t *testing.T) {
	for _, scenario := range []string{"cold", "replaced", "moved_out", "duplicate_name", "partial", "traversal"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			driver := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path != "/file" || r.URL.Query().Get("method") != "list" {
					t.Fatal("ancestry attempted download or another adapter")
				}
				parent := r.URL.Query().Get("parent_fid")
				files := []any{}
				switch parent {
				case "root":
					files = append(files, map[string]any{"fid": "season", "filename": "Season 01", "isdir": 1, "size": 0})
				case "season":
					id := "episode"
					if scenario == "replaced" {
						id = "replacement"
					}
					if scenario != "moved_out" {
						files = append(files, map[string]any{"fid": id, "filename": "Episode.mkv", "isdir": 0, "size": 1024})
					}
					if scenario == "duplicate_name" {
						files = append(files, map[string]any{"fid": "duplicate", "filename": "Episode.mkv", "isdir": 0, "size": 1024})
					}
				default:
					t.Fatal("requested a directory outside the configured root")
				}
				total := len(files)
				if scenario == "partial" && parent == "season" {
					total = 0
				}
				return envelope(r, map[string]any{"total_count": total, "files": files}), nil
			})
			path := "/Season 01/Episode.mkv"
			if scenario == "traversal" {
				path = "/../Episode.mkv"
			}
			item, err := driver.ResolveScopedItem(context.Background(), QualifyID(RouteTV, "tv-account", "root"), path, QualifyID(RouteTV, "tv-account", "episode"))
			if scenario == "cold" {
				if err != nil || item.ParentID != QualifyID(RouteTV, "tv-account", "season") || item.Size != 1024 || calls != 2 {
					t.Fatalf("cold rooted resolution failed: %+v %v calls=%d", item, err, calls)
				}
			} else if err == nil {
				t.Fatal("invalid or changed locator remained playable")
			}
			if scenario == "traversal" && calls != 0 {
				t.Fatal("invalid path reached provider")
			}
		})
	}
}

var _ interface {
	ResolveScopedItem(context.Context, string, string, string) (cloud.Item, error)
} = (*Client)(nil)
