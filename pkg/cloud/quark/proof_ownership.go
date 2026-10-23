package quark

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// OwnsCommonSpaceReceipts identifies only the paired private random challenge
// upload. A reserved filename alone never exempts an ordinary media mutation.
// This is local ownership evidence, not cloud completion/cancellation evidence.
func OwnsCommonSpaceReceipts(proof, upload cloud.OperationReceipt) bool {
	if proof.ID == "" || upload.ID != proof.ID+"/upload" || proof.Provider != cloud.ProviderQuark || upload.Provider != cloud.ProviderQuark || proof.Route != string(RouteAgent) || upload.Route != string(RouteAgent) || proof.Kind != "space_proof" || upload.Kind != "upload" || proof.AccountID != upload.AccountID {
		return false
	}
	var p proofPayload
	var u uploadPayload
	if json.Unmarshal(proof.Payload, &p) != nil || json.Unmarshal(upload.Payload, &u) != nil || len(p.Bytes) != 32 || !filename(p.Name) || !IsVerificationName(p.Name) || !scalar(p.TVAccount, 4096) || u.Name != p.Name || u.Size != 32 || !qualifiedAgentMatches(p.AgentRoot, proof.AccountID, u.Parent) {
		return false
	}
	parts, err := pathSegments(p.Path)
	if err != nil || p.Path != "/"+strings.Join(parts, "/") {
		return false
	}
	if _, err = parseID(p.TVRoot, RouteTV, p.TVAccount, false); err != nil {
		return false
	}
	h := sha256.Sum256(p.Bytes)
	if u.SHA256 != hex.EncodeToString(h[:]) {
		return false
	}
	if u.Target != nil && (!u.Target.Challenge || u.Target.Proof.ReceiptID != proof.ID || u.Target.Path != p.Path || u.Target.TVParent != p.TVRoot || !sameAgent(u.Target.AgentParent, u.Parent) || u.Target.Proof.TVAccount != p.TVAccount || u.Target.Proof.AgentAccount != proof.AccountID) {
		return false
	}
	return true
}
