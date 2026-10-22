package quark

import (
	"context"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// ResolveManagementItem locates a canonical TV item inside a proven scope.
// Bounded fresh directory traversal never substitutes one API's file handle
// into another API. Callers with a known relative path should use the cheaper
// ResolveManagementPath directly.
func (d *Client) ResolveManagementItem(ctx context.Context, tvID string) (cloud.Item, error) {
	d.mu.RLock()
	locator, located := d.directoryLocators[tvID]
	d.mu.RUnlock()
	if located {
		if _, err := d.ResolveScopedDirectory(ctx, locator.rootID, locator.rootPath, locator.relative, tvID); err != nil {
			return cloud.Item{}, err
		}
	}
	c := d.Credentials()
	if c.TV == nil || c.Agent == nil {
		return cloud.Item{}, failure(CodeProof)
	}
	if _, e := parseID(tvID, RouteTV, c.TV.AccountID, false); e != nil {
		return cloud.Item{}, e
	}
	for _, proof := range c.Proofs {
		if !c.HasProof(proof.TVRoot, proof.AgentRoot) {
			continue
		}
		if tvID == proof.TVRoot {
			return d.resolveManagementPath(ctx, proof.TVRoot, proof.AgentRoot, "", tvID)
		}
		if rel, ok := d.observedRelativePath(proof.TVRoot, tvID); ok {
			// Observed parents supply only a path hint. Both routes independently
			// reread every component and the exact TV identity remains pinned.
			return d.resolveManagementPath(ctx, proof.TVRoot, proof.AgentRoot, rel, tvID)
		}
		if located {
			// A frozen locator outside this proof must not initiate a broader scan.
			continue
		}
		type pending struct {
			id, path string
			depth    int
		}
		queue := []pending{{id: proof.TVRoot}}
		seen := map[string]bool{proof.TVRoot: true}
		count := 0
		for i := 0; i < len(queue); i++ {
			if i >= 200 {
				return cloud.Item{}, failure(CodeIncomplete)
			}
			entries, e := d.tvDirectory(ctx, queue[i].id)
			if e != nil {
				return cloud.Item{}, e
			}
			for _, f := range entries {
				count++
				if count > 10000 || seen[f.ID] {
					return cloud.Item{}, failure(CodeIncomplete)
				}
				seen[f.ID] = true
				rel := strings.TrimPrefix(queue[i].path+"/"+f.Name, "/")
				if f.ID == tvID {
					return d.resolveManagementPath(ctx, proof.TVRoot, proof.AgentRoot, rel, tvID)
				}
				if f.IsDir {
					if queue[i].depth >= 127 {
						return cloud.Item{}, failure(CodeIncomplete)
					}
					queue = append(queue, pending{id: f.ID, path: rel, depth: queue[i].depth + 1})
				}
			}
		}
	}
	return cloud.Item{}, failure(CodeProof)
}

func (d *Client) observedRelativePath(root, id string) (string, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	parts, seen := []string{}, map[string]bool{}
	for depth := 0; depth < 128; depth++ {
		if id == root {
			for l, r := 0, len(parts)-1; l < r; l, r = l+1, r-1 {
				parts[l], parts[r] = parts[r], parts[l]
			}
			return strings.Join(parts, "/"), true
		}
		item, ok := d.observed[id]
		if !ok || seen[id] || !filename(item.Name) || item.ParentID == "" {
			return "", false
		}
		seen[id] = true
		parts = append(parts, item.Name)
		id = item.ParentID
	}
	return "", false
}

// ResolveReadItem maps a management result to a fresh canonical TV identity.
// Root, every path component, final type and size must agree independently.
func (d *Client) ResolveReadItem(ctx context.Context, agentID string) (cloud.Item, error) {
	c := d.Credentials()
	if c.Agent == nil || c.TV == nil {
		return cloud.Item{}, failure(CodeProof)
	}
	raw, e := parseID(agentID, RouteAgent, c.Agent.AccountID, false)
	if e != nil {
		return cloud.Item{}, e
	}
	if id := operationID(ctx); id != "" {
		item, handled, err := d.resolveCreatedReadItem(ctx, id, raw)
		if handled {
			return item, err
		}
	}
	for _, proof := range c.Proofs {
		if !c.HasProof(proof.TVRoot, proof.AgentRoot) {
			continue
		}
		root, e := parseID(proof.AgentRoot, RouteAgent, c.Agent.AccountID, false)
		if e != nil {
			continue
		}
		parts := []string{}
		current := raw
		seen := map[string]bool{}
		for depth := 0; depth < 128; depth++ {
			if sameAgent(current, root) {
				for l, r := 0, len(parts)-1; l < r; l, r = l+1, r-1 {
					parts[l], parts[r] = parts[r], parts[l]
				}
				rel := strings.Join(parts, "/")
				mapped, e := d.ResolveManagementPath(ctx, proof.TVRoot, proof.AgentRoot, rel)
				if e != nil {
					return cloud.Item{}, e
				}
				verifiedRaw, e := parseID(mapped.ID, RouteAgent, c.Agent.AccountID, false)
				if e != nil || !sameAgent(verifiedRaw, raw) {
					return cloud.Item{}, failure(CodeIdentity)
				}
				path := strings.TrimSuffix(proof.Path, "/")
				if rel != "" {
					path += "/" + rel
				}
				return d.resolveTV(ctx, path)
			}
			if current == "0" || seen[agentKey(current)] {
				break
			}
			seen[agentKey(current)] = true
			item, e := d.agentInfo(ctx, current)
			if e != nil {
				return cloud.Item{}, e
			}
			parts = append(parts, item.Name)
			current = item.Parent
		}
	}
	return cloud.Item{}, failure(CodeProof)
}

// ResolveUploadReadbackItem returns only a candidate for an immediate complete
// content readback. It revalidates the current confirmed upload's private
// creation mapping without rereading that mapping's content a second time.
// The caller must still verify every byte against its independently frozen
// source digest before publishing or completing delivery. This method neither
// creates mappings nor supplies full_readback evidence. General lookups and
// cold mapping verification must continue to use ResolveReadItem.
func (d *Client) ResolveUploadReadbackItem(ctx context.Context, agentID string) (cloud.Item, error) {
	if err := d.loadRestrictedCredentials(ctx); err != nil {
		return cloud.Item{}, err
	}
	c := d.Credentials()
	if c.Agent == nil || c.TV == nil {
		return cloud.Item{}, failure(CodeProof)
	}
	raw, err := parseID(agentID, RouteAgent, c.Agent.AccountID, false)
	if err != nil {
		return cloud.Item{}, err
	}
	if id := operationID(ctx); id != "" {
		item, handled, err := d.resolveOperationReadItem(ctx, id, raw, true)
		if handled {
			return item, err
		}
	}
	// No eligible cross-domain upload mapping: retain the strict existing
	// resolver, including its receipt and complete-content recovery behavior.
	return d.ResolveReadItem(ctx, agentID)
}
