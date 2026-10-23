package quark

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

type proofPayload struct {
	Path      string        `json:"path"`
	TVRoot    string        `json:"tv_root"`
	AgentRoot string        `json:"agent_root"`
	TVAccount string        `json:"tv_account"`
	Name      string        `json:"name"`
	Bytes     []byte        `json:"bytes"`
	Proof     *SpaceProof   `json:"proof,omitempty"`
	Challenge *cloud.Item   `json:"challenge,omitempty"`
	Cleanup   *proofCleanup `json:"cleanup,omitempty"`
}

// VerifyCommonSpace proves access to one managed root using independent TV
// traversal and complete anonymous readback of an Agent-created random file.
// It does NOT claim account IDs from different applications are equivalent.
// The fixed receipt ID must survive retries. Uncertain upload outcomes block
// new creation; verified challenge files remain excluded from scans/adoption.
func (d *Client) VerifyCommonSpace(ctx context.Context, id, path string) (result SpaceProof, resultErr error) {
	stage := "verification_input"
	defer func() { resultErr = operationError(stage, resultErr) }()
	if d.config.Operations == nil || d.config.PersistCredentials == nil || !scalar(id, 96) {
		return SpaceProof{}, failure(CodeUnsupported)
	}
	parts, e := pathSegments(path)
	if e != nil {
		return SpaceProof{}, e
	}
	path = "/" + strings.Join(parts, "/")
	stage = "playback_authorization"
	tvAuth, e := d.authorization(ctx, RouteTV)
	if e != nil {
		return SpaceProof{}, e
	}
	stage = "management_authorization"
	agentAuth, e := d.authorization(ctx, RouteAgent)
	if e != nil {
		return SpaceProof{}, e
	}
	stage = "playback_directory"
	tv, e := d.resolveTV(ctx, path)
	if e != nil {
		return SpaceProof{}, e
	}
	stage = "management_directory"
	agent, e := d.resolveAgent(ctx, path)
	if e != nil {
		return SpaceProof{}, e
	}
	if !tv.IsDir || !agent.Dir {
		return SpaceProof{}, failure(CodeProof)
	}
	agentRoot := QualifyID(RouteAgent, agentAuth.AccountID, agent.ID)
	p := proofPayload{Path: path, TVRoot: tv.ID, AgentRoot: agentRoot, TVAccount: tvAuth.AccountID}
	stage = "proof_receipt_load"
	r, e := d.config.Operations.Load(ctx, id)
	if e == nil {
		if r.Provider != cloud.ProviderQuark || r.Route != string(RouteAgent) || r.AccountID != agentAuth.AccountID || r.Kind != "space_proof" || json.Unmarshal(r.Payload, &p) != nil || p.Path != path || p.TVRoot != tv.ID || !qualifiedAgentMatches(p.AgentRoot, agentAuth.AccountID, agent.ID) || p.TVAccount != tvAuth.AccountID || len(p.Bytes) != 32 {
			return SpaceProof{}, failure(CodeIdentity)
		}
		// Retain the original receipt's handle for the frozen upload request.
		agentRoot = p.AgentRoot
		if r.State == "confirmed" && p.Proof != nil {
			stage = "proof_credentials_save"
			return *p.Proof, d.finishProof(ctx, id, *p.Proof)
		}
	} else {
		// A failed or unreadable checkpoint is not affirmative absence. Never
		// manufacture a replacement challenge when the original is unresolved.
		if !errors.Is(e, cloud.ErrOperationNotFound) {
			return SpaceProof{}, e
		}
		stage = "proof_challenge_prepare"
		p.Name = ".omc-verify-" + randomID()
		p.Bytes = make([]byte, 32)
		if _, e = rand.Read(p.Bytes); e != nil {
			return SpaceProof{}, failure(CodeUnavailable)
		}
		r = cloud.OperationReceipt{ID: id, Provider: cloud.ProviderQuark, Route: string(RouteAgent), AccountID: agentAuth.AccountID, Kind: "space_proof", Revision: 1, State: "prepared", UpdatedAt: d.api.clock()}
		r.Payload, _ = json.Marshal(p)
		stage = "proof_receipt_create"
		if e = d.config.Operations.Create(ctx, r); e != nil {
			return SpaceProof{}, e
		}
	}
	// Exactly one upload receipt is permanently bound to this proof attempt.
	stage = "challenge_upload"
	_, e = d.upload(ctx, id+"/upload", cloud.UploadRequest{ParentID: p.AgentRoot, Name: p.Name, Size: 32, Reader: bytes.NewReader(p.Bytes)}, true)
	if e != nil {
		return SpaceProof{}, e
	}
	if r.State == "prepared" {
		stage = "proof_receipt_save"
		next := r
		next.State = "accepted"
		next.Revision++
		next.UpdatedAt = d.api.clock()
		if e = d.config.Operations.Update(ctx, r.Revision, next); e != nil {
			return SpaceProof{}, e
		}
		r = next
	}
	stage = "challenge_directory"
	entries, e := d.tvDirectory(ctx, tv.ID)
	if e != nil {
		return SpaceProof{}, e
	}
	var found cloud.Item
	count := 0
	for _, f := range entries {
		if f.Name == p.Name {
			count++
			found = f
		}
	}
	if count != 1 || found.IsDir || found.Size != 32 {
		return SpaceProof{}, failure(CodePending)
	}
	stage = "challenge_readback"
	read, e := d.OpenRead(ctx, cloud.ReadRequest{FileID: found.ID})
	if e != nil {
		return SpaceProof{}, e
	}
	defer read.Body.Close()
	actual, e := io.ReadAll(io.LimitReader(read.Body, 33))
	if e != nil || !bytes.Equal(actual, p.Bytes) {
		return SpaceProof{}, failure(CodeProof)
	}
	hash := sha256.Sum256(p.Bytes)
	proof := SpaceProof{Version: 1, Generation: r.Revision, TVAccount: tvAuth.AccountID, AgentAccount: agentAuth.AccountID, TVRoot: tv.ID, AgentRoot: agentRoot, Path: path, ReceiptID: id, ContentSHA256: hex.EncodeToString(hash[:]), VerifiedAt: d.api.clock().UTC()}
	p.Proof = &proof
	p.Challenge = &found
	next := r
	next.Revision++
	next.State = "confirmed"
	next.UpdatedAt = d.api.clock()
	next.Payload, _ = json.Marshal(p)
	stage = "proof_receipt_save"
	if e = d.config.Operations.Update(ctx, r.Revision, next); e != nil {
		return SpaceProof{}, e
	}
	stage = "proof_credentials_save"
	if e = d.finishProof(ctx, id, proof); e != nil {
		return proof, e
	}
	return proof, nil
}

func (d *Client) finishProof(ctx context.Context, id string, proof SpaceProof) error {
	if err := d.persistProof(ctx, proof); err != nil {
		return err
	}
	// Cleanup failure is independent of the durable access proof. Its receipt
	// remains pending and can be retried without replaying verification/upload.
	_, _ = d.CleanupCommonSpace(ctx, id)
	return nil
}
func (d *Client) persistProof(ctx context.Context, p SpaceProof) error {
	// Proof I/O may itself refresh authorization, so only hold the shared lock
	// here, after I/O and before loading/CAS-updating the credential bundle.
	lock := d.credentialLock()
	lock.Lock()
	defer lock.Unlock()
	if err := checkCommonSpaceGuard(ctx); err != nil {
		return err
	}
	c := d.Credentials()
	if d.config.LoadCredentials != nil {
		raw, e := d.config.LoadCredentials(ctx)
		if e != nil {
			return e
		}
		c, e = ParseCredentials(raw)
		if e != nil {
			return e
		}
	}
	if c.TV == nil || c.Agent == nil || !c.TV.IdentityVerified || !c.Agent.IdentityVerified || c.TV.AccountID != p.TVAccount || c.Agent.AccountID != p.AgentAccount {
		return failure(CodeIdentity)
	}
	for _, existing := range c.Proofs {
		if existing.ReceiptID == p.ReceiptID && existing.ContentSHA256 == p.ContentSHA256 {
			d.mu.Lock()
			d.credentials = c
			d.mu.Unlock()
			return nil
		}
	}
	next := cloneCredentials(c)
	proofs := []SpaceProof{}
	for _, existing := range next.Proofs {
		if existing.TVRoot != p.TVRoot || existing.AgentRoot != p.AgentRoot {
			proofs = append(proofs, existing)
		}
	}
	next.Proofs = append(proofs, p)
	next.Revision++
	raw, _ := json.Marshal(next)
	if e := d.config.PersistCredentials(ctx, c.Revision, raw); e != nil {
		return e
	}
	d.mu.Lock()
	d.credentials = next
	d.mu.Unlock()
	return nil
}

// Verification artifacts have an explicit provider-reserved name and private
// creation receipt. Consumers skip them, never adopt/delete a user file merely
// because its name has this prefix.
func IsVerificationName(name string) bool { return strings.HasPrefix(name, ".omc-verify-") }
