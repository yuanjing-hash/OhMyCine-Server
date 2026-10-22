package quark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// TV and management deliberately use different IDs for the same physical tree.
func selectionFixture(t *testing.T) (*Client, *memoryOperations, *int, *int, *bool) {
	t.Helper()
	mkdirs, saves := 0, 0
	unavailable := false
	files := map[string]agentItem{"A": {ID: "A", Parent: "0", Name: "library", Dir: true}}
	tvID := func(id string) string {
		if id == "A" {
			return "T"
		}
		if id == "0" {
			return "0"
		}
		return "tv-" + id
	}
	d, store := managedFixture(t, func(r *http.Request) (*http.Response, error) {
		if unavailable {
			return nil, errors.New("publisher removed and network unavailable")
		}
		switch r.URL.Path {
		case "/file":
			if r.URL.Query().Get("method") != "list" {
				t.Fatal("unexpected TV download")
			}
			list := []any{}
			for _, f := range files {
				if tvID(f.Parent) == r.URL.Query().Get("parent_fid") {
					kind := 0
					if f.Dir {
						kind = 1
					}
					list = append(list, map[string]any{"fid": tvID(f.ID), "parent_fid": tvID(f.Parent), "filename": f.Name, "isdir": kind, "size": f.Size})
				}
			}
			return envelope(r, map[string]any{"total_count": len(list), "files": list}), nil
		case "/open/v1/file/list":
			var body struct {
				Parent string `json:"parent_fid"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			list := []any{}
			for _, f := range files {
				if f.Parent == body.Parent {
					list = append(list, agentFile(f.ID, f.Parent, f.Name, f.Dir, f.Size))
				}
			}
			return envelope(r, map[string]any{"last_page": true, "file_list": list}), nil
		case "/open/v1/file/info":
			f, ok := files[r.URL.Query().Get("fid")]
			if !ok {
				t.Fatalf("unknown management item")
			}
			return envelope(r, agentFile(f.ID, f.Parent, f.Name, f.Dir, f.Size)), nil
		case "/open/v1/dir":
			mkdirs++
			var body struct {
				Parent string `json:"pdir_fid"`
				Name   string `json:"dir_path"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			id := fmt.Sprintf("dir-%d", mkdirs)
			files[id] = agentItem{ID: id, Parent: body.Parent, Name: body.Name, Dir: true}
			return envelope(r, map[string]any{"fid": id}), nil
		case "/1/clouddrive/share/sharepage/v2/detail", "/1/clouddrive/share/sharepage/detail":
			list := []any{map[string]any{"fid": "one", "file_name": "one.mkv", "size": 22, "file_type": 1, "share_fid_token": "one-secret"}, map[string]any{"fid": "two", "file_name": "two.mkv", "size": 44, "file_type": 1, "share_fid_token": "two-secret"}}
			data := map[string]any{"list": list, "detail_info": map[string]any{"list": list}, "token_info": map[string]any{"stoken": "guest-secret"}}
			return response(r, 200, map[string]any{"status": 200, "code": 0, "data": data}), nil
		case "/open/v1/share/saveas":
			saves++
			var body struct {
				Parent string   `json:"to_pdir_fid"`
				IDs    []string `json:"fid_list"`
				Tokens []string `json:"fid_token_list"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if !reflect.DeepEqual(body.IDs, []string{"one"}) || !reflect.DeepEqual(body.Tokens, []string{"one-secret"}) {
				t.Fatal("unselected share file submitted")
			}
			files["saved-one"] = agentItem{ID: "saved-one", Parent: body.Parent, Name: "one.mkv", Size: 22}
			return envelope(r, map[string]any{"finish": true}), nil
		}
		t.Fatalf("unexpected route %s", r.URL.Path)
		return nil, nil
	})
	return d, store, &mkdirs, &saves, &unavailable
}

func TestReceiveSelectionFreezesActualOutputAndSurvivesPublisherRemoval(t *testing.T) {
	d, store, mkdirs, saves, unavailable := selectionFixture(t)
	ctx := context.Background()
	selected := &cloud.ShareSelection{Version: 1, Files: []cloud.ShareTreeItem{{ID: QualifyID(Route("share"), "abc123", "one"), RelativePath: "one.mkv", Size: 22}}}
	result, e := d.ReceiveSelection(ctx, "omc-task", "https://pan.quark.cn/s/abc123", QualifyID(RouteTV, "tv-account", "T"), "omc-task", selected)
	if e != nil || !result.Complete || len(result.Entries) != 1 || result.Entries[0].RelativePath != "one.mkv" || result.Entries[0].ID != QualifyID(RouteTV, "tv-account", "tv-saved-one") {
		t.Fatalf("result %+v error %v", result, e)
	}
	if *mkdirs != 1 || *saves != 1 || store.receipts["omc-task"].State != "confirmed" {
		t.Fatal("not one durable receive")
	}
	*unavailable = true
	raw, _ := json.Marshal(d.Credentials())
	restarted, e := NewClient(cloud.Config{Credentials: raw, Operations: store})
	if e != nil {
		t.Fatal(e)
	}
	restarted.api.http.Transport = d.api.http.Transport
	again, e := restarted.GetSelection(ctx, "omc-task")
	if e != nil || !reflect.DeepEqual(result, again) || *mkdirs != 1 || *saves != 1 {
		t.Fatal("confirmed output still depends on share or new mutation", e)
	}
	wrong := *selected
	wrong.Files = append([]cloud.ShareTreeItem(nil), selected.Files...)
	wrong.Files[0].Size++
	_, e = restarted.ReceiveSelection(ctx, "omc-task", "https://pan.quark.cn/s/abc123", QualifyID(RouteTV, "tv-account", "T"), "omc-task", &wrong)
	errorCode(t, e, CodeIdentity)
}

func TestIntakeAdoptionFreezesManifestWithoutAnotherCloudWrite(t *testing.T) {
	d, _, mkdirs, saves, unavailable := selectionFixture(t)
	ctx := context.Background()
	selected := &cloud.ShareSelection{Version: 1, Files: []cloud.ShareTreeItem{{ID: QualifyID(Route("share"), "abc123", "one"), RelativePath: "one.mkv", Size: 22}}}
	saved, e := d.ReceiveSelection(ctx, "omc-existing", "https://pan.quark.cn/s/abc123", QualifyID(RouteTV, "tv-account", "T"), "omc-existing", selected)
	if e != nil {
		t.Fatal(e)
	}
	adopted, e := d.AdoptSelection(ctx, "omc-intake", QualifyID(RouteTV, "tv-account", "T"), saved.Root.ID)
	if e != nil || !adopted.Complete || !reflect.DeepEqual(saved.Entries, adopted.Entries) {
		t.Fatal("adoption did not freeze actual manifest", e)
	}
	if *mkdirs != 1 || *saves != 1 {
		t.Fatal("adoption created another provider operation")
	}
	*unavailable = true
	again, e := d.GetSelection(ctx, "omc-intake")
	if e != nil || !reflect.DeepEqual(adopted, again) {
		t.Fatal("adoption did not survive unavailable provider", e)
	}
}
