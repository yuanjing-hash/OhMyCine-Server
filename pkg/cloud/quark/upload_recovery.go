package quark

import (
	"context"
	"errors"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

func uploadSubmitFailure(err error) *operationSubmitFailure {
	var f *Failure
	if !errors.As(err, &f) {
		return nil
	}
	diagnostic := &operationSubmitFailure{}
	if f.HTTPStatus >= 100 && f.HTTPStatus <= 599 {
		diagnostic.HTTPStatus = f.HTTPStatus
	}
	if f.Errno >= -999999999 && f.Errno <= 999999999 {
		diagnostic.Errno = f.Errno
	}
	return diagnostic
}

func uploadUnknownError(p uploadPayload) error {
	stage := "upload_output_confirmation"
	switch p.Phase {
	case "prepare", "pre_inflight":
		stage = "upload_prepare"
	case "put_inflight":
		stage = "upload_part"
	case "finish_inflight", "finish_received":
		stage = "upload_finish"
	}
	f := &Failure{Code: cloud.CodeMutationUnknown, UnknownOutcome: true}
	if p.SubmitFailure != nil {
		f.HTTPStatus, f.Errno = p.SubmitFailure.HTTPStatus, p.SubmitFailure.Errno
	}
	return operationError(stage, f)
}

// A random challenge has stronger ownership evidence than an ordinary upload:
// its private receipt froze both the unpredictable name and all 32 bytes before
// submission. Recover only that exact artifact, with independent complete TV
// readback and fresh Agent metadata. Absence never permits another upload.
func (d *Client) recoverChallengeUpload(ctx context.Context, r *cloud.OperationReceipt, p uploadPayload) (cloud.Item, error) {
	if p.Target == nil || !p.Target.Challenge || p.Size != 32 || !IsVerificationName(p.Name) || !strings.HasSuffix(r.ID, "/upload") || r.ID != p.Target.Proof.ReceiptID+"/upload" || p.Phase != "pre_inflight" && p.Phase != "finish_inflight" {
		return cloud.Item{}, uploadUnknownError(p)
	}
	target, err := d.freezeChallengeTarget(ctx, r.ID, p.Parent, p.Name, p.SHA256)
	if err != nil {
		return cloud.Item{}, operationError("upload_output_confirmation", err)
	}
	if target.Path != p.Target.Path || target.TVParent != p.Target.TVParent || target.Proof.TVAccount != p.Target.Proof.TVAccount || target.Proof.AgentAccount != r.AccountID || !sameAgent(target.AgentParent, p.Target.AgentParent) {
		return cloud.Item{}, failure(CodeIdentity)
	}
	if _, err = d.creationTarget(ctx, target); err != nil {
		return cloud.Item{}, operationError("upload_output_confirmation", err)
	}
	entries, err := d.agentDirectory(ctx, p.Parent)
	if err != nil {
		return cloud.Item{}, operationError("upload_output_confirmation", err)
	}
	var found agentItem
	count := 0
	for _, item := range entries {
		if item.Name == p.Name {
			found, count = item, count+1
		}
	}
	if count == 0 {
		return cloud.Item{}, uploadUnknownError(p)
	}
	if count != 1 || found.Dir || found.Size != p.Size || !sameAgent(found.Parent, p.Parent) {
		return cloud.Item{}, failure(CodeIdentity)
	}
	info, err := d.agentInfo(ctx, found.ID)
	if err != nil {
		return cloud.Item{}, operationError("upload_output_confirmation", err)
	}
	// confirmCreationMapping also rejects contradictory same-domain IDs and
	// requires a unique TV child with the same name, size and all content bytes.
	p.Mapping, err = d.confirmCreationMapping(ctx, *r, target, info, found, p.SHA256, nil)
	if err != nil {
		return cloud.Item{}, operationError("upload_output_confirmation", err)
	}
	p.Handle, p.Phase = found.ID, "metadata_confirmed"
	output := toCloudAgent(r.AccountID, info)
	p.Output = &output
	if err = d.saveUpload(ctx, r, p, "confirmed", false); err != nil {
		return cloud.Item{}, err
	}
	return output, nil
}
