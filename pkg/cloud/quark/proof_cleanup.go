package quark

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// This private receipt is not a general delete/recycle capability. The target
// is the exact Agent child independently proved against the random TV bytes.
type proofCleanup struct {
	State   string                  `json:"state"`
	Agent   cloud.Item              `json:"agent"`
	TV      cloud.Item              `json:"tv"`
	Failure *operationSubmitFailure `json:"failure,omitempty"`
}

func (d *Client) ProofCleanupState(ctx context.Context, id string) (string, error) {
	r, p, err := d.loadCleanupProof(ctx, id)
	if err != nil {
		return "pending", err
	}
	if r.State == "confirmed" && p.Cleanup != nil && p.Cleanup.State == "complete" {
		return "complete", nil
	}
	return "pending", nil
}

func (d *Client) loadCleanupProof(ctx context.Context, id string) (cloud.OperationReceipt, proofPayload, error) {
	var p proofPayload
	if d.config.Operations == nil {
		return cloud.OperationReceipt{}, p, failure(CodeUnsupported)
	}
	r, err := d.config.Operations.Load(ctx, id)
	if err != nil {
		return r, p, operationError("proof_receipt_load", err)
	}
	c := d.Credentials()
	if r.Provider != cloud.ProviderQuark || r.Route != string(RouteAgent) || r.Kind != "space_proof" || r.State != "confirmed" || json.Unmarshal(r.Payload, &p) != nil || p.Proof == nil || len(p.Bytes) != 32 || !filename(p.Name) || !IsVerificationName(p.Name) || c.TV == nil || c.Agent == nil || r.AccountID != c.Agent.AccountID || p.TVAccount != c.TV.AccountID {
		return r, p, failure(CodeIdentity)
	}
	h := sha256.Sum256(p.Bytes)
	proof := p.Proof
	if proof.ReceiptID != id || proof.TVAccount != p.TVAccount || proof.AgentAccount != r.AccountID || proof.Path != p.Path || proof.TVRoot != p.TVRoot || proof.AgentRoot != p.AgentRoot || proof.ContentSHA256 != hex.EncodeToString(h[:]) || !c.HasProof(proof.TVRoot, proof.AgentRoot) {
		return r, p, failure(CodeIdentity)
	}
	for _, saved := range c.Proofs {
		if saved == *proof {
			return r, p, nil
		}
	}
	return r, p, failure(CodeIdentity)
}

func (d *Client) saveProofCleanup(ctx context.Context, r *cloud.OperationReceipt, p proofPayload, checkpoint bool) error {
	if checkpoint {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
	}
	next := *r
	next.Revision++
	next.UpdatedAt = d.api.clock()
	next.Payload, _ = json.Marshal(p)
	if err := d.config.Operations.Update(ctx, r.Revision, next); err != nil {
		return operationError("proof_receipt_save", err)
	}
	*r = next
	return nil
}

// CleanupCommonSpace operates only on a durably saved, still-current proof.
// After dispatch every restart is read-only, including a missing response.
// Two complete route listings establish that the exact saved child is no
// longer present in the selected root; this is not a claim of permanent purge.
func (d *Client) CleanupCommonSpace(ctx context.Context, id string) (state string, resultErr error) {
	defer func() { resultErr = operationError("challenge_cleanup", resultErr) }()
	if _, err := d.authorization(ctx, RouteTV); err != nil {
		return "pending", err
	}
	a, err := d.authorization(ctx, RouteAgent)
	if err != nil {
		return "pending", err
	}
	r, p, err := d.loadCleanupProof(ctx, id)
	if err != nil {
		return "pending", err
	}
	if p.Cleanup != nil && p.Cleanup.State == "complete" {
		return "complete", nil
	}
	upload, err := d.config.Operations.Load(ctx, id+"/upload")
	if err != nil {
		return "pending", operationError("upload_receipt_load", err)
	}
	var u uploadPayload
	if upload.Provider != cloud.ProviderQuark || upload.Route != string(RouteAgent) || upload.Kind != "upload" || upload.State != "confirmed" || upload.AccountID != a.AccountID || json.Unmarshal(upload.Payload, &u) != nil || u.Output == nil || u.Output.Name != p.Name || u.Output.Size != 32 || u.Output.IsDir || u.Target == nil || !u.Target.Challenge || u.Target.Proof.ReceiptID != id || u.Target.Path != p.Path || u.Target.TVParent != p.TVRoot || u.Name != p.Name || u.Size != 32 || u.SHA256 != p.Proof.ContentSHA256 || !qualifiedAgentMatches(p.AgentRoot, a.AccountID, u.Parent) || !qualifiedAgentMatches(u.Output.ID, a.AccountID, u.Handle) {
		return "pending", failure(CodeIdentity)
	}
	if _, err = d.creationTarget(ctx, u.Target); err != nil {
		return "pending", err
	}
	expectedAgent := u.Handle
	if u.Mapping != nil {
		if u.Mapping.Version != 1 || u.Mapping.Operation != upload.ID || u.Mapping.ReceiptRevision != upload.Revision || u.Mapping.ACK != u.Handle || u.Mapping.SHA256 != u.SHA256 {
			return "pending", failure(CodeIdentity)
		}
		expectedAgent = u.Mapping.Listed
	}
	if p.Cleanup != nil && (!qualifiedAgentMatches(p.Cleanup.Agent.ID, a.AccountID, expectedAgent) || p.Cleanup.Agent.Name != p.Name || p.Cleanup.Agent.Size != 32 || p.Cleanup.Agent.IsDir || !qualifiedAgentMatches(p.Cleanup.Agent.ParentID, a.AccountID, u.Parent) || p.Cleanup.TV.Name != p.Name || p.Cleanup.TV.Size != 32 || p.Cleanup.TV.IsDir || p.Cleanup.TV.ParentID != p.TVRoot || p.Challenge != nil && p.Cleanup.TV.ID != p.Challenge.ID || u.Mapping != nil && p.Cleanup.TV.ID != u.Mapping.TV.ID) {
		return "pending", failure(CodeIdentity)
	}
	agents, err := d.agentDirectory(ctx, u.Parent)
	if err != nil {
		return "pending", err
	}
	tvs, err := d.tvDirectory(ctx, p.TVRoot)
	if err != nil {
		return "pending", err
	}
	if p.Cleanup != nil {
		if p.Cleanup.State != "prepared" && p.Cleanup.State != "submitting" && p.Cleanup.State != "accepted" && p.Cleanup.State != "unknown" {
			return "pending", failure(CodeIdentity)
		}
		// Never retarget a same-name replacement. On a lost delete ACK we only
		// query the frozen identities; no second deletion is sent.
		agentPresent, tvPresent := false, false
		for _, item := range agents {
			if qualifiedAgentMatches(p.Cleanup.Agent.ID, a.AccountID, item.ID) {
				agentPresent = true
			}
		}
		for _, item := range tvs {
			if item.ID == p.Cleanup.TV.ID {
				tvPresent = true
			}
		}
		if !agentPresent && !tvPresent {
			p.Cleanup.State = "complete"
			if err = d.saveProofCleanup(ctx, &r, p, false); err != nil {
				return "pending", err
			}
			return "complete", nil
		}
		if p.Cleanup.State != "prepared" {
			f := &Failure{Code: cloud.CodeMutationUnknown, UnknownOutcome: true}
			if p.Cleanup.Failure != nil {
				f.HTTPStatus, f.Errno = p.Cleanup.Failure.HTTPStatus, p.Cleanup.Failure.Errno
			}
			return "pending", f
		}
	}
	var agent agentItem
	var tv cloud.Item
	ac, tc := 0, 0
	for _, item := range agents {
		if item.Name == p.Name {
			agent, ac = item, ac+1
		}
	}
	for _, item := range tvs {
		if item.Name == p.Name {
			tv, tc = item, tc+1
		}
	}
	if ac != 1 || tc != 1 || agent.Dir || tv.IsDir || agent.Size != 32 || tv.Size != 32 || tv.ParentID != p.TVRoot || !sameAgent(agent.Parent, u.Parent) {
		return "pending", failure(CodeIdentity)
	}
	if u.Mapping != nil {
		if u.Mapping.TV.ID != tv.ID {
			return "pending", failure(CodeIdentity)
		}
	}
	if !sameAgent(expectedAgent, agent.ID) || p.Challenge != nil && p.Challenge.ID != tv.ID || p.Cleanup != nil && (!qualifiedAgentMatches(p.Cleanup.Agent.ID, a.AccountID, agent.ID) || p.Cleanup.TV.ID != tv.ID) {
		return "pending", failure(CodeIdentity)
	}
	info, err := d.agentInfo(ctx, agent.ID)
	if err != nil {
		return "pending", err
	}
	if info.Name != p.Name || info.Dir || info.Size != 32 || !sameAgent(info.Parent, u.Parent) {
		return "pending", failure(CodeIdentity)
	}
	read, err := d.OpenRead(ctx, cloud.ReadRequest{FileID: tv.ID})
	if err != nil {
		return "pending", err
	}
	if read.Body == nil {
		return "pending", failure(CodeInvalid)
	}
	actual, err := io.ReadAll(io.LimitReader(read.Body, 33))
	_ = read.Body.Close()
	if err != nil || !bytes.Equal(actual, p.Bytes) {
		return "pending", failure(CodeIdentity)
	}
	if p.Cleanup == nil {
		p.Cleanup = &proofCleanup{State: "prepared", Agent: toCloudAgent(a.AccountID, agent), TV: tv}
		if err = d.saveProofCleanup(ctx, &r, p, false); err != nil {
			return "pending", err
		}
	}
	if err = d.dispatchProofCleanup(ctx, &r, p, a, agent.ID); err != nil {
		return "pending", err
	}
	// The saved accepted state makes this one bounded continuation query-only.
	return d.CleanupCommonSpace(ctx, id)
}

func (d *Client) dispatchProofCleanup(ctx context.Context, r *cloud.OperationReceipt, p proofPayload, a Authorization, handle string) error {
	// Recheck the latest persisted credential bundle under its shared lock and
	// hold it until dispatch/checkpoint. Account replacement cannot cross the
	// final destructive boundary, while same-account token rotation is allowed.
	lock := d.credentialLock()
	lock.Lock()
	defer lock.Unlock()
	if d.config.LoadCredentials != nil {
		raw, err := d.config.LoadCredentials(ctx)
		if err != nil {
			return err
		}
		latest, err := ParseCredentials(raw)
		if err != nil {
			return err
		}
		d.mu.Lock()
		d.credentials = latest
		d.mu.Unlock()
	}
	if _, _, err := d.loadCleanupProof(ctx, r.ID); err != nil {
		return err
	}
	current := d.Credentials().Agent
	if current == nil || current.AccountID != a.AccountID {
		return failure(CodeIdentity)
	}
	p.Cleanup.State = "submitting"
	if err := d.saveProofCleanup(ctx, r, p, false); err != nil {
		return err
	}
	// Protocol: OpenList QuarkOpen Remove at 2caa4f5d, /open/v1/file/delete.
	// An HTTP ACK alone proves neither entitlement nor completed removal.
	_, err := d.api.agent(ctx, *current, http.MethodPost, "/open/v1/file/delete", nil, map[string]any{"action_type": 1, "fid_list": []string{handle}})
	if err != nil {
		p.Cleanup.State, p.Cleanup.Failure = "unknown", uploadSubmitFailure(err)
		_ = d.saveProofCleanup(ctx, r, p, true)
		return unknownUploadOutcome(err)
	}
	p.Cleanup.State = "accepted"
	return d.saveProofCleanup(ctx, r, p, true)
}
