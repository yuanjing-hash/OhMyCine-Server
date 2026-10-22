package quark

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func TestColdManagementDirectoryLocatorUsesOnlyFrozenPath(t *testing.T) {
	for _, scenario := range []string{"cold", "root_replaced", "directory_replaced"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			d, _ := managedFixture(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path == "/file" {
					parent := r.URL.Query().Get("parent_fid")
					files := []any{}
					add := func(id, name string) {
						files = append(files, map[string]any{"fid": id, "filename": name, "isdir": 1, "size": 0})
					}
					switch parent {
					case "0":
						id := "T"
						if scenario == "root_replaced" {
							id = "other"
						}
						add(id, "library")
					case "T":
						for i := 0; i < 25; i++ {
							add(fmt.Sprintf("unused-%d", i), fmt.Sprintf("unused-%d", i))
						}
						add("nested", "nested")
					case "nested":
						id := "I"
						if scenario == "directory_replaced" {
							id = "other"
						}
						add(id, "intake")
					default:
						t.Fatalf("enumerated unrelated subtree: %s", parent)
					}
					return envelope(r, map[string]any{"total_count": len(files), "files": files}), nil
				}
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				file := agentFile("A", "0", "library", true, 0)
				switch body["parent_fid"] {
				case "A":
					file = agentFile("agent-nested", "A", "nested", true, 0)
				case "agent-nested":
					file = agentFile("agent-intake", "agent-nested", "intake", true, 0)
				}
				return envelope(r, map[string]any{"last_page": true, "file_list": []any{file}}), nil
			})
			root, target := QualifyID(RouteTV, "tv-account", "T"), QualifyID(RouteTV, "tv-account", "I")
			if err := d.SetDirectoryLocator(root, "/library", "/nested/intake", target); err != nil {
				t.Fatal(err)
			}
			item, err := d.ResolveManagementItem(context.Background(), target)
			if scenario != "cold" {
				errorCode(t, err, CodeIdentity)
				return
			}
			if err != nil || item.ID != QualifyID(RouteAgent, "agent-account", "agent-intake") {
				t.Fatalf("cold directory mapping: %v", err)
			}
			if calls > 12 {
				t.Fatalf("path mapping unexpectedly expanded: %d", calls)
			}
		})
	}
}

func TestDirectoryLocatorRejectsUnrootedOrTraversalHints(t *testing.T) {
	d, _ := managedFixture(t, func(r *http.Request) (*http.Response, error) {
		t.Fatal("invalid locator reached network")
		return nil, nil
	})
	for _, paths := range [][2]string{{"", "/intake"}, {"/library", ""}, {"/library", "/../outside"}, {"relative", "/intake"}} {
		if err := d.SetDirectoryLocator("root", paths[0], paths[1], "target"); err == nil {
			t.Fatal("invalid locator accepted")
		}
	}
}
