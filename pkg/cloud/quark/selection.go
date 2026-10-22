package quark

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"sort"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

type SelectionResult struct {
	Root     cloud.Item        `json:"root"`
	Entries  []cloud.TreeEntry `json:"entries"`
	Complete bool              `json:"complete"`
}
type selectionPayload struct {
	URL       string               `json:"url"`
	Parent    string               `json:"parent"`
	Name      string               `json:"name"`
	Selection cloud.ShareSelection `json:"selection"`
	Result    SelectionResult      `json:"result"`
}

func childOperation(id, purpose string) string {
	h := sha256.Sum256([]byte(id + "\x00" + purpose))
	return "qs-" + hex.EncodeToString(h[:])
}
func (d *Client) saveSelection(ctx context.Context, r *cloud.OperationReceipt, p selectionPayload, state string) error {
	next := *r
	next.Revision++
	next.State = state
	next.UpdatedAt = d.api.clock()
	next.Payload, _ = json.Marshal(p)
	if e := d.config.Operations.Update(ctx, r.Revision, next); e != nil {
		return e
	}
	*r = next
	return nil
}

// ReceiveSelection persists the entire exact leaf selection before creating a
// task directory. Each directory/save call has its own stable durable receipt.
// Confirmed outputs are independent of the publisher's continued availability.
func (d *Client) ReceiveSelection(ctx context.Context, id, raw, tvParent, name string, requested *cloud.ShareSelection) (SelectionResult, error) {
	if d.config.Operations == nil || !scalar(id, 128) || !strings.HasPrefix(name, "omc-") || !filename(name) {
		return SelectionResult{}, failure(CodeInvalid)
	}
	normalized, _, e := NormalizeShareLink(raw, "")
	if e != nil {
		return SelectionResult{}, e
	}
	a, e := d.authorization(ctx, RouteAgent)
	if e != nil {
		return SelectionResult{}, e
	}
	p := selectionPayload{URL: normalized, Parent: tvParent, Name: name}
	r, e := d.config.Operations.Load(ctx, id)
	if e == nil {
		if r.Provider != cloud.ProviderQuark || r.Route != string(RouteAgent) || r.AccountID != a.AccountID || r.Kind != "share_selection" || json.Unmarshal(r.Payload, &p) != nil || p.URL != normalized || p.Parent != tvParent || p.Name != name {
			return SelectionResult{}, failure(CodeIdentity)
		}
		if requested != nil {
			want, err := requested.Digest()
			have, _ := p.Selection.Digest()
			if err != nil || want != have {
				return SelectionResult{}, failure(CodeIdentity)
			}
		}
		if r.State == "confirmed" && p.Result.Complete {
			return p.Result, nil
		}
		if r.State != "prepared" && r.State != "accepted" {
			return SelectionResult{}, failure(cloud.CodeMutationUnknown)
		}
	} else {
		_, tree, err := cloud.InspectShareTree(ctx, d, normalized)
		if err != nil {
			return SelectionResult{}, err
		}
		p.Selection = cloud.ShareSelection{Version: 1}
		if requested != nil {
			p.Selection = *requested
		} else {
			for _, f := range tree {
				if !f.IsDir {
					p.Selection.Files = append(p.Selection.Files, f)
				}
			}
		}
		if err := p.Selection.Validate(); err != nil {
			return SelectionResult{}, failure("quark_share_selection_invalid")
		}
		current := map[string]cloud.ShareTreeItem{}
		for _, f := range tree {
			current[f.RelativePath] = f
		}
		for _, f := range p.Selection.Files {
			if current[f.RelativePath] != f {
				return SelectionResult{}, failure("quark_share_changed")
			}
		}
		r = cloud.OperationReceipt{ID: id, Provider: cloud.ProviderQuark, Route: string(RouteAgent), AccountID: a.AccountID, Kind: "share_selection", Revision: 1, State: "prepared", UpdatedAt: d.api.clock()}
		r.Payload, _ = json.Marshal(p)
		if e = d.config.Operations.Create(ctx, r); e != nil {
			return SelectionResult{}, e
		}
	}
	if r.State == "prepared" {
		if e = d.saveSelection(ctx, &r, p, "accepted"); e != nil {
			return SelectionResult{}, e
		}
	}
	rootOp := childOperation(id, "task-root")
	root, err := d.existingOrDirectory(ctx, rootOp, tvParent, name, true)
	if err != nil {
		return SelectionResult{}, err
	}
	dirs := map[string]string{".": root.ID}
	groups := map[string][]cloud.ShareTreeItem{}
	desired := map[string]bool{".": true}
	for _, f := range p.Selection.Files {
		dir := path.Dir(f.RelativePath)
		groups[dir] = append(groups[dir], f)
		for dir != "." {
			desired[dir] = true
			dir = path.Dir(dir)
		}
	}
	ordered := []string{}
	for dir := range desired {
		ordered = append(ordered, dir)
	}
	sort.Strings(ordered)
	for _, dir := range ordered {
		if dir != "." {
			created, err := d.existingOrDirectory(ctx, childOperation(id, "dir:"+dir), dirs[path.Dir(dir)], path.Base(dir), false)
			if err != nil {
				return SelectionResult{}, err
			}
			dirs[dir] = created.ID
		}
		files := groups[dir]
		sort.Slice(files, func(i, j int) bool { return files[i].RelativePath < files[j].RelativePath })
		for start := 0; start < len(files); start += 100 {
			end := min(start+100, len(files))
			batchID := childOperation(id, "save:"+dir+":"+files[start].ID)
			if old, err := d.config.Operations.Load(ctx, batchID); err == nil {
				if old.Kind != "receive_share" || old.AccountID != a.AccountID {
					return SelectionResult{}, failure(CodeIdentity)
				}
				result, err := d.QueryOperation(ctx, batchID)
				if err != nil {
					return SelectionResult{}, err
				}
				if !result.Complete {
					return SelectionResult{}, failure(CodePending)
				}
				continue
			}
			snapshot, err := d.shareAtRelative(ctx, normalized, dir)
			if err != nil {
				return SelectionResult{}, err
			}
			byID := map[string]cloud.ShareItem{}
			for _, f := range snapshot.Items {
				byID[f.ID] = f
			}
			snapshot.Items = nil
			for _, wanted := range files[start:end] {
				f, ok := byID[wanted.ID]
				if !ok || f.IsDir || f.Name != path.Base(wanted.RelativePath) || f.Size != wanted.Size {
					return SelectionResult{}, failure("quark_share_changed")
				}
				snapshot.Items = append(snapshot.Items, f)
			}
			private, err := selectedShare(&snapshot)
			if err != nil {
				return SelectionResult{}, err
			}
			snapshot.Private, _ = json.Marshal(private)
			result, err := d.BeginOperation(ctx, cloud.OperationRequest{ID: batchID, Kind: "receive_share", ParentID: dirs[dir], Share: &snapshot})
			if err != nil {
				return SelectionResult{}, err
			}
			if !result.Complete {
				return SelectionResult{}, failure(CodePending)
			}
		}
	}
	// Enumerate the actual resulting TV tree, never the original share. Both
	// route mappings verify independent unique positions, type and size first.
	tvRoot, e := d.ResolveReadItem(ctx, root.ID)
	if e != nil {
		return SelectionResult{}, e
	}
	result := SelectionResult{Root: tvRoot, Complete: true}
	tvDirs := map[string]string{".": tvRoot.ID}
	want := map[string]cloud.ShareTreeItem{}
	for _, f := range p.Selection.Files {
		want[f.RelativePath] = f
	}
	for _, dir := range ordered {
		if tvDirs[dir] == "" {
			return SelectionResult{}, failure(CodePending)
		}
		items, e := d.tvDirectory(ctx, tvDirs[dir])
		if e != nil {
			return SelectionResult{}, e
		}
		for _, f := range items {
			rel := f.Name
			if dir != "." {
				rel = dir + "/" + f.Name
			}
			if f.IsDir {
				if !desired[rel] || tvDirs[rel] != "" {
					return SelectionResult{}, failure(CodeIdentity)
				}
				tvDirs[rel] = f.ID
				continue
			}
			wanted, ok := want[rel]
			if !ok || wanted.Size != f.Size {
				return SelectionResult{}, failure(CodeIdentity)
			}
			delete(want, rel)
			result.Entries = append(result.Entries, cloud.TreeEntry{Item: f, RelativePath: rel})
		}
	}
	if len(want) != 0 {
		return SelectionResult{}, failure(CodePending)
	}
	p.Result = result
	if e = d.saveSelection(ctx, &r, p, "confirmed"); e != nil {
		return SelectionResult{}, e
	}
	return result, nil
}
func (d *Client) existingOrDirectory(ctx context.Context, id, parent, name string, tv bool) (cloud.Item, error) {
	if _, e := d.config.Operations.Load(ctx, id); e == nil {
		r, e := d.QueryOperation(ctx, id)
		if e != nil {
			return cloud.Item{}, e
		}
		if !r.Complete || len(r.Items) != 1 {
			return cloud.Item{}, failure(CodePending)
		}
		return r.Items[0], nil
	}
	if tv {
		mapped, e := d.ResolveManagementItem(ctx, parent)
		if e != nil {
			return cloud.Item{}, e
		}
		parent = mapped.ID
	}
	r, e := d.BeginOperation(ctx, cloud.OperationRequest{ID: id, Kind: "mkdir", ParentID: parent, Name: name})
	if e != nil {
		return cloud.Item{}, e
	}
	if !r.Complete || len(r.Items) != 1 {
		return cloud.Item{}, failure(CodePending)
	}
	return r.Items[0], nil
}
func (d *Client) shareAtRelative(ctx context.Context, raw, relative string) (cloud.ShareSnapshot, error) {
	s, e := d.InspectShare(ctx, raw)
	if e != nil || relative == "." {
		return s, e
	}
	for _, part := range strings.Split(relative, "/") {
		id := ""
		for _, f := range s.Items {
			if f.Name == part && f.IsDir {
				if id != "" {
					return cloud.ShareSnapshot{}, failure(CodeIdentity)
				}
				id = f.ID
			}
		}
		if id == "" {
			return cloud.ShareSnapshot{}, failure("quark_share_changed")
		}
		s, e = d.InspectShareDirectory(ctx, raw, id)
		if e != nil {
			return cloud.ShareSnapshot{}, e
		}
	}
	return s, nil
}
func (d *Client) GetSelection(ctx context.Context, id string) (SelectionResult, error) {
	if d.config.Operations == nil {
		return SelectionResult{}, failure(CodeUnsupported)
	}
	r, e := d.config.Operations.Load(ctx, id)
	if e != nil {
		return SelectionResult{}, e
	}
	if r.Kind == "intake_selection" {
		var p adoptionPayload
		if json.Unmarshal(r.Payload, &p) != nil {
			return SelectionResult{}, failure(CodeIdentity)
		}
		return d.AdoptSelection(ctx, id, p.Parent, p.Item)
	}
	var p selectionPayload
	if r.Kind != "share_selection" || json.Unmarshal(r.Payload, &p) != nil {
		return SelectionResult{}, failure(CodeIdentity)
	}
	return d.ReceiveSelection(ctx, id, p.URL, p.Parent, p.Name, &p.Selection)
}
