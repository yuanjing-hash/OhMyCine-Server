package quark

import (
	"context"
	"encoding/json"
	"path"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

type adoptionPayload struct {
	Parent string          `json:"parent"`
	Item   string          `json:"item"`
	Result SelectionResult `json:"result"`
}

// AdoptSelection freezes an existing intake item without copying or receiving
// anything. The caller's private intake claim supplies the canonical TV item.
func (d *Client) AdoptSelection(ctx context.Context, id, parent, itemID string) (SelectionResult, error) {
	if d.config.Operations == nil || !scalar(id, 128) {
		return SelectionResult{}, failure(CodeInvalid)
	}
	a, e := d.authorization(ctx, RouteTV)
	if e != nil {
		return SelectionResult{}, e
	}
	p := adoptionPayload{Parent: parent, Item: itemID}
	r, e := d.config.Operations.Load(ctx, id)
	if e == nil {
		if r.Provider != cloud.ProviderQuark || r.Route != string(RouteTV) || r.AccountID != a.AccountID || r.Kind != "intake_selection" || json.Unmarshal(r.Payload, &p) != nil || p.Parent != parent || p.Item != itemID {
			return SelectionResult{}, failure(CodeIdentity)
		}
		if r.State == "confirmed" && p.Result.Complete {
			return p.Result, nil
		}
		if r.State != "prepared" && r.State != "accepted" {
			return SelectionResult{}, failure(cloud.CodeMutationUnknown)
		}
	} else {
		r = cloud.OperationReceipt{ID: id, Provider: cloud.ProviderQuark, Route: string(RouteTV), AccountID: a.AccountID, Kind: "intake_selection", Revision: 1, State: "prepared", UpdatedAt: d.api.clock()}
		r.Payload, _ = json.Marshal(p)
		if e = d.config.Operations.Create(ctx, r); e != nil {
			return SelectionResult{}, e
		}
	}
	if r.State == "prepared" {
		next := r
		next.Revision++
		next.State = "accepted"
		next.UpdatedAt = d.api.clock()
		if e = d.config.Operations.Update(ctx, r.Revision, next); e != nil {
			return SelectionResult{}, e
		}
		r = next
	}
	// Read only the frozen intake parent's direct children. This primes the
	// candidate's path on a cold driver and rejects a moved/replaced candidate.
	if _, e = d.ResolveManagementItem(ctx, parent); e != nil {
		return SelectionResult{}, e
	}
	children, e := d.tvDirectory(ctx, parent)
	if e != nil {
		return SelectionResult{}, e
	}
	found := false
	for _, item := range children {
		if item.ID == itemID && item.ParentID == parent {
			found = true
		}
	}
	if !found {
		return SelectionResult{}, failure(CodeIdentity)
	}
	// Mapping revalidates the independently proven root and populates fresh TV
	// ancestors. No management mutation is issued by this read-only operation.
	if _, e = d.ResolveManagementItem(ctx, itemID); e != nil {
		return SelectionResult{}, e
	}
	root, e := d.Stat(ctx, itemID)
	if e != nil {
		return SelectionResult{}, e
	}
	if root.ID != itemID || root.ParentID != parent {
		return SelectionResult{}, failure(CodeIdentity)
	}
	result := SelectionResult{Root: root, Complete: true}
	if !root.IsDir {
		result.Entries = []cloud.TreeEntry{{Item: root, RelativePath: root.Name}}
	} else {
		type folder struct{ id, relative string }
		queue := []folder{{root.ID, ""}}
		seen := map[string]bool{root.ID: true}
		dirs := 0
		for len(queue) > 0 {
			f := queue[0]
			queue = queue[1:]
			dirs++
			if dirs > 200 {
				return SelectionResult{}, failure("quark_intake_too_large")
			}
			items, err := d.tvDirectory(ctx, f.id)
			if err != nil {
				return SelectionResult{}, err
			}
			for _, item := range items {
				if seen[item.ID] || item.ParentID != f.id {
					return SelectionResult{}, failure(CodeIdentity)
				}
				seen[item.ID] = true
				if len(seen) > 2001 {
					return SelectionResult{}, failure("quark_intake_too_large")
				}
				rel := path.Join(f.relative, item.Name)
				if item.IsDir {
					queue = append(queue, folder{item.ID, rel})
				} else {
					result.Entries = append(result.Entries, cloud.TreeEntry{Item: item, RelativePath: rel})
				}
			}
		}
	}
	if len(result.Entries) == 0 {
		return SelectionResult{}, failure("quark_intake_empty")
	}
	p.Result = result
	next := r
	next.Revision++
	next.State = "confirmed"
	next.UpdatedAt = d.api.clock()
	next.Payload, _ = json.Marshal(p)
	if e = d.config.Operations.Update(ctx, r.Revision, next); e != nil {
		return SelectionResult{}, e
	}
	return result, nil
}
