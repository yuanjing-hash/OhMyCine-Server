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
	diagnostic.Reason = safeUploadSubmitReason(f.SubmitReason)
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
	return operationError(stage, uploadUnknownFailure(p))
}

func uploadUnknownFailure(p uploadPayload) error {
	f := &Failure{Code: cloud.CodeMutationUnknown, UnknownOutcome: true}
	if p.SubmitFailure != nil {
		f.HTTPStatus, f.Errno = p.SubmitFailure.HTTPStatus, p.SubmitFailure.Errno
		f.SubmitReason = safeUploadSubmitReason(p.SubmitFailure.Reason)
	}
	return f
}

// Older challenge receipts can lack the mapping target. Reconstruct only from
// the original private proof (same account, root, name and all random bytes),
// then validate the current root before saving it to the same upload receipt.
func (d *Client) restoreChallengeTarget(ctx context.Context, r *cloud.OperationReceipt, p *uploadPayload) error {
	if p.Target != nil {
		return nil
	}
	if p.Size != 32 || !IsVerificationName(p.Name) || p.Mapping != nil {
		return failure(CodeIdentity)
	}
	target, err := d.freezeChallengeTarget(ctx, r.ID, p.Parent, p.Name, p.SHA256)
	if err != nil {
		return operationError("upload_legacy_recovery", err)
	}
	if _, err = d.creationTarget(ctx, target); err != nil {
		return operationError("upload_legacy_recovery", err)
	}
	p.Target = target
	return d.saveUpload(ctx, r, *p, r.State, false)
}

// A random challenge has stronger ownership evidence than an ordinary upload:
// its private receipt froze both the unpredictable name and all 32 bytes before
// submission. Recover only that exact artifact, with independent complete TV
// readback and fresh Agent metadata. Absence never permits another upload.
func (d *Client) recoverChallengeUpload(ctx context.Context, r *cloud.OperationReceipt, p uploadPayload) (result cloud.Item, resultErr error) {
	defer func() { resultErr = withUploadDiagnostic(*r, p, resultErr) }()
	target, err := d.originalChallengeTarget(ctx, r, p)
	if err != nil {
		return cloud.Item{}, err
	}
	var taskErr error
	if p.TaskID != "" {
		var status uploadTaskStatus
		status, taskErr = d.readUploadTask(ctx, &p)
		if taskErr != nil && challengeTaskConflict(p) {
			return cloud.Item{}, taskErr
		}
		if taskErr == nil && status.Handle != "" {
			p.Handle, p.InFlightPart, p.Phase = status.Handle, 0, "finish_received"
			if err = d.saveUpload(ctx, r, p, "accepted", false); err != nil {
				return cloud.Item{}, err
			}
			item, err := d.confirmUpload(ctx, r, p)
			return item, operationError("upload_output_confirmation", err)
		}
		if taskErr == nil && p.Phase == "put_inflight" {
			p.Observation.Reason = "original_part_unconfirmed"
			for _, part := range status.Parts {
				if part.Number == p.InFlightPart {
					p.Observation.Reason = "task_unfinished"
				}
			}
			taskErr = operationError("upload_part_recovery", uploadUnknownFailure(p))
		}
	}
	item, err := d.confirmChallengeOutput(ctx, r, p, target)
	if code, _ := cloud.ErrorInfo(err); code == cloud.CodeMutationUnknown && taskErr != nil {
		return cloud.Item{}, taskErr
	}
	return item, err
}

// A missing original part is not permission to repeat its PUT. Only the
// original private challenge can be recovered from independent complete file
// evidence; ordinary media and a missing challenge retain the original error.
func (d *Client) recoverChallengePartOutput(ctx context.Context, r *cloud.OperationReceipt, p uploadPayload, taskErr error) (result cloud.Item, resultErr error) {
	defer func() { resultErr = withUploadDiagnostic(*r, p, resultErr) }()
	if ctx.Err() != nil || p.Phase != "put_inflight" || p.Observation == nil || p.Observation.Pages == 0 || challengeTaskConflict(p) {
		return cloud.Item{}, taskErr
	}
	target, err := d.originalChallengeTarget(ctx, r, p)
	if err != nil {
		return cloud.Item{}, err
	}
	item, err := d.confirmChallengeOutput(ctx, r, p, target)
	if code, _ := cloud.ErrorInfo(err); code == cloud.CodeMutationUnknown {
		return cloud.Item{}, taskErr
	}
	return item, err
}

func challengeTaskConflict(p uploadPayload) bool {
	if p.Observation == nil {
		return false
	}
	switch p.Observation.Reason {
	case "part_changed", "finish_status_regressed", "completed_fid_changed":
		return true
	}
	return false
}

func (d *Client) originalChallengeTarget(ctx context.Context, r *cloud.OperationReceipt, p uploadPayload) (*operationTarget, error) {
	if p.Target == nil || !p.Target.Challenge || p.Size != 32 || !IsVerificationName(p.Name) || !strings.HasSuffix(r.ID, "/upload") || r.ID != p.Target.Proof.ReceiptID+"/upload" || p.Phase != "pre_inflight" && p.Phase != "put_inflight" && p.Phase != "finish_inflight" {
		return nil, uploadUnknownError(p)
	}
	target, err := d.freezeChallengeTarget(ctx, r.ID, p.Parent, p.Name, p.SHA256)
	if err != nil {
		return nil, operationError("upload_output_confirmation", err)
	}
	if target.Path != p.Target.Path || target.TVParent != p.Target.TVParent || target.Proof.TVAccount != p.Target.Proof.TVAccount || target.Proof.AgentAccount != r.AccountID || !sameAgent(target.AgentParent, p.Target.AgentParent) {
		return nil, failure(CodeIdentity)
	}
	if _, err = d.creationTarget(ctx, target); err != nil {
		return nil, operationError("upload_output_confirmation", err)
	}
	return target, nil
}

func (d *Client) confirmChallengeOutput(ctx context.Context, r *cloud.OperationReceipt, p uploadPayload, target *operationTarget) (cloud.Item, error) {
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
	p.Handle, p.InFlightPart, p.Phase = found.ID, 0, "metadata_confirmed"
	output := toCloudAgent(r.AccountID, info)
	p.Output = &output
	if err = d.saveUpload(ctx, r, p, "confirmed", false); err != nil {
		return cloud.Item{}, err
	}
	return output, nil
}
