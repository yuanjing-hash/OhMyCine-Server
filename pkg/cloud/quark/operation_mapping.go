package quark

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// operationTarget freezes a creation's destination before submission. It is
// encrypted with that operation's receipt, never an alias table for arbitrary
// provider IDs. An ACK and a listing handle remain distinct namespaces.
type operationTarget struct {
	Proof       SpaceProof `json:"proof"`
	Path        string     `json:"path"`
	AgentParent string     `json:"agent_parent"`
	TVParent    string     `json:"tv_parent"`
	Challenge   bool       `json:"challenge,omitempty"`
}
type operationMapping struct {
	Version         int        `json:"version"`
	Operation       string     `json:"operation"`
	ReceiptRevision uint64     `json:"receipt_revision"`
	ACK             string     `json:"ack"`
	Listed          string     `json:"listed"`
	TV              cloud.Item `json:"tv"`
	SHA256          string     `json:"sha256,omitempty"`
}

func sameAgentDomain(a, b string) bool {
	return strings.HasPrefix(agentKey(a), "fingerprint:") == strings.HasPrefix(agentKey(b), "fingerprint:")
}

func (d *Client) freezeCreationTarget(ctx context.Context, parent string) (*operationTarget, error) {
	c := d.Credentials()
	if c.Agent == nil || c.TV == nil {
		return nil, failure(CodeProof)
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
		current := parent
		seen := map[string]bool{}
		for depth := 0; depth < 128; depth++ {
			if sameAgent(current, root) {
				for l, r := 0, len(parts)-1; l < r; l, r = l+1, r-1 {
					parts[l], parts[r] = parts[r], parts[l]
				}
				path := strings.TrimSuffix(proof.Path, "/")
				if len(parts) > 0 {
					path += "/" + strings.Join(parts, "/")
				}
				if path == "" {
					path = "/"
				}
				tvParent := proof.TVRoot
				if len(parts) > 0 {
					item, e := d.resolveTV(ctx, path)
					if e != nil {
						return nil, e
					}
					if !item.IsDir {
						return nil, failure(CodeIdentity)
					}
					tvParent = item.ID
				}
				return &operationTarget{Proof: proof, Path: path, AgentParent: parent, TVParent: tvParent}, nil
			}
			if current == "0" || seen[agentKey(current)] {
				break
			}
			seen[agentKey(current)] = true
			item, e := d.agentInfo(ctx, current)
			if e != nil {
				return nil, e
			}
			if !item.Dir {
				return nil, failure(CodeIdentity)
			}
			parts = append(parts, item.Name)
			current = item.Parent
		}
	}
	return nil, failure(CodeProof)
}

func (d *Client) freezeChallengeTarget(ctx context.Context, id, parent, name, sha string) (*operationTarget, error) {
	if !strings.HasSuffix(id, "/upload") {
		return nil, failure(CodeProof)
	}
	r, e := d.config.Operations.Load(ctx, strings.TrimSuffix(id, "/upload"))
	if e != nil {
		return nil, e
	}
	c := d.Credentials()
	var p proofPayload
	if c.Agent == nil || c.TV == nil || r.Provider != cloud.ProviderQuark || r.Route != string(RouteAgent) || r.AccountID != c.Agent.AccountID || r.Kind != "space_proof" || r.State != "prepared" && r.State != "accepted" && r.State != "confirmed" || json.Unmarshal(r.Payload, &p) != nil || p.Name != name || len(p.Bytes) != 32 || p.TVAccount != c.TV.AccountID || p.AgentRoot != QualifyID(RouteAgent, c.Agent.AccountID, parent) {
		return nil, failure(CodeProof)
	}
	h := sha256.Sum256(p.Bytes)
	if hex.EncodeToString(h[:]) != sha {
		return nil, failure(CodeIdentity)
	}
	proof := SpaceProof{Version: 1, Generation: 1, TVAccount: c.TV.AccountID, AgentAccount: c.Agent.AccountID, TVRoot: p.TVRoot, AgentRoot: p.AgentRoot, Path: p.Path, ReceiptID: r.ID}
	return &operationTarget{Proof: proof, Path: p.Path, AgentParent: parent, TVParent: p.TVRoot, Challenge: true}, nil
}

// Verify the current authorizations and every frozen root/path before accepting
// or reusing a creation mapping. Token refresh alone does not change identity.
func (d *Client) creationTarget(ctx context.Context, target *operationTarget) (cloud.Item, error) {
	c := d.Credentials()
	if target == nil || c.TV == nil || c.Agent == nil || !c.TV.IdentityVerified || !c.Agent.IdentityVerified || target.Proof.TVAccount != c.TV.AccountID || target.Proof.AgentAccount != c.Agent.AccountID {
		return cloud.Item{}, failure(CodeProof)
	}
	if _, err := pathSegments(target.Path); err != nil || target.AgentParent == "" || target.TVParent == "" || target.Path != target.Proof.Path && !strings.HasPrefix(target.Path, strings.TrimSuffix(target.Proof.Path, "/")+"/") {
		return cloud.Item{}, failure(CodeProof)
	}
	if target.Challenge {
		r, e := d.config.Operations.Load(ctx, target.Proof.ReceiptID)
		var p proofPayload
		if e != nil || r.Provider != cloud.ProviderQuark || r.Kind != "space_proof" || r.Route != string(RouteAgent) || r.AccountID != c.Agent.AccountID || r.State != "prepared" && r.State != "accepted" && r.State != "confirmed" || json.Unmarshal(r.Payload, &p) != nil || p.Path != target.Path || p.TVRoot != target.TVParent || p.AgentRoot != target.Proof.AgentRoot || p.TVAccount != c.TV.AccountID || len(p.Bytes) != 32 {
			return cloud.Item{}, failure(CodeProof)
		}
	} else {
		found := false
		for _, p := range c.Proofs {
			if p.ReceiptID == target.Proof.ReceiptID && p.Generation == target.Proof.Generation && p.TVRoot == target.Proof.TVRoot && p.AgentRoot == target.Proof.AgentRoot && p.Path == target.Proof.Path && c.HasProof(p.TVRoot, p.AgentRoot) {
				found = true
				break
			}
		}
		if !found {
			return cloud.Item{}, failure(CodeProof)
		}
	}
	tvRoot, e := d.resolveTV(ctx, target.Proof.Path)
	if e != nil {
		return cloud.Item{}, e
	}
	agentRoot, e := d.resolveAgent(ctx, target.Proof.Path)
	if e != nil {
		return cloud.Item{}, e
	}
	if tvRoot.ID != target.Proof.TVRoot || !tvRoot.IsDir || !agentRoot.Dir || !qualifiedAgentMatches(target.Proof.AgentRoot, c.Agent.AccountID, agentRoot.ID) {
		return cloud.Item{}, failure(CodeIdentity)
	}
	tvParent, agentParent := tvRoot, agentRoot
	if target.Path != target.Proof.Path {
		tvParent, e = d.resolveTV(ctx, target.Path)
		if e != nil {
			return cloud.Item{}, e
		}
		agentParent, e = d.resolveAgent(ctx, target.Path)
		if e != nil {
			return cloud.Item{}, e
		}
	}
	if tvParent.ID != target.TVParent || !tvParent.IsDir || !agentParent.Dir || !sameAgent(agentParent.ID, target.AgentParent) {
		return cloud.Item{}, failure(CodeIdentity)
	}
	return tvParent, nil
}

func (d *Client) confirmCreationMapping(ctx context.Context, r cloud.OperationReceipt, target *operationTarget, ack, listed agentItem, expected string, previous *operationMapping) (*operationMapping, error) {
	mapping, e := d.creationMappingCandidate(ctx, r, target, ack, listed, expected, previous)
	if e != nil {
		return nil, e
	}
	found := mapping.TV
	if !found.IsDir {
		stream, e := d.OpenRead(ctx, cloud.ReadRequest{FileID: found.ID})
		if e != nil {
			return nil, e
		}
		if stream.Body == nil {
			return nil, failure(CodeInvalid)
		}
		defer stream.Body.Close()
		if stream.TotalSize != nil && *stream.TotalSize != found.Size {
			return nil, failure(CodeIdentity)
		}
		h := sha256.New()
		size, e := io.Copy(h, io.LimitReader(contextReader{ctx, stream.Body}, found.Size+1))
		if e != nil {
			return nil, e
		}
		if size != found.Size || hex.EncodeToString(h.Sum(nil)) != expected {
			return nil, failure(CodeIdentity)
		}
	}
	return mapping, nil
}

// This is metadata/receipt validation only. Its result is never persisted or
// treated as current content proof without confirmCreationMapping's complete
// read, or the upload caller's mandatory independent delivery readback.
func (d *Client) creationMappingCandidate(ctx context.Context, r cloud.OperationReceipt, target *operationTarget, ack, listed agentItem, expected string, previous *operationMapping) (*operationMapping, error) {
	parent, e := d.creationTarget(ctx, target)
	if e != nil {
		return nil, e
	}
	if r.AccountID != target.Proof.AgentAccount || r.Provider != cloud.ProviderQuark || r.Route != string(RouteAgent) || r.Kind != "upload" && r.Kind != "mkdir" || ack.Name != listed.Name || ack.Dir != listed.Dir || ack.Size != listed.Size || !sameAgent(listed.Parent, target.AgentParent) || sameAgentDomain(ack.ID, listed.ID) && !sameAgent(ack.ID, listed.ID) {
		return nil, failure(CodeIdentity)
	}
	if !sameAgent(ack.Parent, target.AgentParent) {
		// Never ignore a contradiction within an ID namespace. For distinct
		// namespaces, independently confirm the ACK's asserted parent metadata;
		// the exact destination is proved separately by the frozen scope and
		// complete TV readback (or the acknowledged mkdir creation receipt).
		if sameAgentDomain(ack.Parent, target.AgentParent) {
			return nil, failure(CodeIdentity)
		}
		p, e := d.agentInfo(ctx, ack.Parent)
		if e != nil {
			return nil, e
		}
		if !p.Dir || p.Name != parent.Name {
			return nil, failure(CodeIdentity)
		}
	}
	entries, e := d.tvDirectory(ctx, parent.ID)
	if e != nil {
		return nil, e
	}
	var found cloud.Item
	n := 0
	for _, f := range entries {
		if f.Name == ack.Name {
			found = f
			n++
		}
	}
	if n != 1 {
		return nil, failure(CodePending)
	}
	if found.IsDir != ack.Dir || found.Size != ack.Size || found.ParentID != parent.ID {
		return nil, failure(CodeIdentity)
	}
	if previous != nil && (previous.Version != 1 || previous.Operation != r.ID || previous.ReceiptRevision != r.Revision || previous.ACK != ack.ID || !sameAgent(previous.Listed, listed.ID) || previous.TV.ID != found.ID || previous.TV.ParentID != found.ParentID || previous.SHA256 != expected) {
		return nil, failure(CodeIdentity)
	}
	if !found.IsDir {
		if r.Kind != "upload" || len(expected) != 64 || found.Size <= 0 || found.Size == int64(^uint64(0)>>1) {
			return nil, failure(CodeIdentity)
		}
	} else if r.Kind != "mkdir" || expected != "" {
		return nil, failure(CodeIdentity)
	}
	revision := r.Revision + 1
	if previous != nil {
		revision = previous.ReceiptRevision
	}
	return &operationMapping{Version: 1, Operation: r.ID, ReceiptRevision: revision, ACK: ack.ID, Listed: listed.ID, TV: found, SHA256: expected}, nil
}

// Only an explicitly named private operation can authorize a cross-domain
// lookup. A fresh Client reloads the receipt; no in-memory alias is trusted.
func (d *Client) resolveCreatedReadItem(ctx context.Context, id, raw string) (cloud.Item, bool, error) {
	return d.resolveOperationReadItem(ctx, id, raw, false)
}

func (d *Client) resolveOperationReadItem(ctx context.Context, id, raw string, immediateUploadReadback bool) (cloud.Item, bool, error) {
	if d.config.Operations == nil {
		return cloud.Item{}, false, nil
	}
	r, e := d.config.Operations.Load(ctx, id)
	if e != nil {
		return cloud.Item{}, true, e
	}
	c := d.Credentials()
	if c.Agent == nil || r.Provider != cloud.ProviderQuark || r.Route != string(RouteAgent) || r.AccountID != c.Agent.AccountID || r.State != "confirmed" {
		return cloud.Item{}, true, failure(CodeIdentity)
	}
	var target *operationTarget
	var mapping *operationMapping
	var name, hash string
	var size int64
	var directory bool
	var uploadOutput *cloud.Item
	switch r.Kind {
	case "upload":
		var p uploadPayload
		if json.Unmarshal(r.Payload, &p) != nil || p.Output == nil || p.Handle != raw || p.Output.ID != QualifyID(RouteAgent, c.Agent.AccountID, raw) {
			return cloud.Item{}, true, failure(CodeIdentity)
		}
		target, mapping, name, hash, size = p.Target, p.Mapping, p.Name, p.SHA256, p.Size
		uploadOutput = p.Output
		if immediateUploadReadback && mapping != nil {
			digest, err := hex.DecodeString(p.SHA256)
			if r.ID != id || r.Revision == 0 || p.Phase != "metadata_confirmed" || target == nil || target.Challenge || !sameAgent(p.Parent, target.AgentParent) || p.Output.Name != p.Name || p.Output.Size != p.Size || p.Output.IsDir || len(digest) != sha256.Size || err != nil || hex.EncodeToString(digest) != p.SHA256 {
				return cloud.Item{}, true, failure(CodeIdentity)
			}
		}
	case "mkdir":
		if immediateUploadReadback {
			return cloud.Item{}, false, nil
		}
		var p operationPayload
		if json.Unmarshal(r.Payload, &p) != nil || p.Handle != raw || len(p.Outputs) != 1 || p.Outputs[0].ID != QualifyID(RouteAgent, c.Agent.AccountID, raw) {
			return cloud.Item{}, true, failure(CodeIdentity)
		}
		target, mapping, name, directory, size = p.Target, p.Mapping, p.Request.Name, true, p.Outputs[0].Size
	default:
		return cloud.Item{}, false, nil
	}
	// Same-domain historical receipts continue through the strict generic path.
	if mapping == nil {
		return cloud.Item{}, false, nil
	}
	ack, e := d.agentInfo(ctx, raw)
	if e != nil {
		return cloud.Item{}, true, e
	}
	if ack.Name != name || ack.Size != size || ack.Dir != directory {
		return cloud.Item{}, true, failure(CodeIdentity)
	}
	if immediateUploadReadback && (uploadOutput == nil || !qualifiedAgentMatches(uploadOutput.ParentID, c.Agent.AccountID, ack.Parent)) {
		return cloud.Item{}, true, failure(CodeIdentity)
	}
	if target == nil {
		return cloud.Item{}, true, failure(CodeProof)
	}
	entries, e := d.agentDirectory(ctx, target.AgentParent)
	if e != nil {
		return cloud.Item{}, true, e
	}
	var listed agentItem
	n := 0
	for _, f := range entries {
		if f.Name == name {
			listed = f
			n++
		}
	}
	if n != 1 {
		return cloud.Item{}, true, failure(CodeIdentity)
	}
	if immediateUploadReadback {
		candidate, err := d.creationMappingCandidate(ctx, r, target, ack, listed, hash, mapping)
		if err != nil {
			return cloud.Item{}, true, err
		}
		// Historical mapping content is not fresh proof. Pin all recorded TV
		// metadata, including a provider-supplied revision, while the caller
		// performs its mandatory current full readback before completion.
		if mapping.TV.Name != candidate.TV.Name || mapping.TV.IsDir != candidate.TV.IsDir || mapping.TV.Size != candidate.TV.Size || mapping.TV.Revision != candidate.TV.Revision {
			return cloud.Item{}, true, failure(CodeIdentity)
		}
		return candidate.TV, true, nil
	}
	verified, e := d.confirmCreationMapping(ctx, r, target, ack, listed, hash, mapping)
	if e != nil {
		return cloud.Item{}, true, e
	}
	return verified.TV, true, nil
}
