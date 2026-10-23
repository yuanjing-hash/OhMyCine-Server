package quark

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// CommonSpaceRecoveryState only inspects existing private checkpoints. It does
// not refresh authorization, contact the provider or create/change a receipt.
// The opaque version fences the two checkpoints inspected by an administrator;
// it is not a provider task ID or evidence that an old write was cancelled.
func (d *Client) CommonSpaceRecoveryState(ctx context.Context, id, path, tvRoot string) (bool, string, error) {
	if d.config.Operations == nil {
		return false, "", failure(CodeUnsupported)
	}
	c, err := d.currentCredentials(ctx)
	if err != nil {
		return false, "", err
	}
	if c.TV == nil || c.Agent == nil || !c.TV.IdentityVerified || !c.Agent.IdentityVerified {
		return false, "", failure(CodeAuth)
	}
	for _, proof := range c.Proofs {
		if proof.TVRoot == tvRoot && proof.Path == path && c.HasProof(proof.TVRoot, proof.AgentRoot) {
			return false, "", nil
		}
	}
	r, err := d.config.Operations.Load(ctx, id)
	if errors.Is(err, cloud.ErrOperationNotFound) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	var p proofPayload
	if r.Provider != cloud.ProviderQuark || r.Route != string(RouteAgent) || r.AccountID != c.Agent.AccountID || r.Kind != "space_proof" || json.Unmarshal(r.Payload, &p) != nil || p.Path != path || p.TVRoot != tvRoot || p.TVAccount != c.TV.AccountID || len(p.Bytes) != 32 || !filename(p.Name) || !IsVerificationName(p.Name) {
		return false, "", failure(CodeIdentity)
	}
	if _, err = parseID(p.AgentRoot, RouteAgent, c.Agent.AccountID, false); err != nil {
		return false, "", err
	}
	if r.State == "confirmed" || p.Proof != nil || r.State != "prepared" && r.State != "accepted" {
		return false, "", nil
	}
	u, err := d.config.Operations.Load(ctx, id+"/upload")
	if errors.Is(err, cloud.ErrOperationNotFound) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	var upload uploadPayload
	h := sha256.Sum256(p.Bytes)
	if u.Provider != cloud.ProviderQuark || u.Route != string(RouteAgent) || u.AccountID != c.Agent.AccountID || u.Kind != "upload" || json.Unmarshal(u.Payload, &upload) != nil || !qualifiedAgentMatches(p.AgentRoot, c.Agent.AccountID, upload.Parent) || upload.Name != p.Name || upload.Size != 32 || upload.SHA256 != hex.EncodeToString(h[:]) {
		return false, "", failure(CodeIdentity)
	}
	if !OwnsCommonSpaceReceipts(r, u) {
		return false, "", failure(CodeIdentity)
	}
	if upload.Handle != "" || upload.Output != nil || u.State != "unknown" && u.State != "accepted" && u.State != "submitting" {
		return false, "", nil
	}
	switch upload.Phase {
	case "pre_inflight", "put_inflight", "finish_inflight":
	default:
		return false, "", nil
	}
	version := sha256.Sum256([]byte(strconv.FormatUint(r.Revision, 10) + "\x00" + strconv.FormatUint(u.Revision, 10)))
	return true, hex.EncodeToString(version[:]), nil
}
