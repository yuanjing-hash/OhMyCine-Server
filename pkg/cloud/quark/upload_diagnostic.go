package quark

import (
	"errors"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// UploadDiagnostic is a bounded projection, never the private upload payload.
// It deliberately contains no task/receipt/file ID, name, URL, hash or ETag.
type UploadDiagnostic struct {
	Phase, State                                         string
	HasTask, HasOutput, TargetRecorded, NotDispatched    bool
	InFlightPart, ConfirmedParts                         int
	TaskFinishStatus                                     *int
	TaskPages                                            int
	TaskListingComplete                                  bool
	TaskQueryReason, TaskResponseField, TaskResponseType string
}

type uploadStateError struct {
	diagnostic UploadDiagnostic
	cause      error
}

func (e *uploadStateError) Error() string { return "quark_upload_failed" }
func (e *uploadStateError) Unwrap() error { return e.cause }

func withUploadDiagnostic(r cloud.OperationReceipt, p uploadPayload, err error) error {
	if err == nil || r.ID == "" {
		return err
	}
	var existing *uploadStateError
	if errors.As(err, &existing) {
		return err
	}
	d := UploadDiagnostic{Phase: "unknown", State: "unknown", HasTask: p.TaskID != "", HasOutput: p.Handle != "", TargetRecorded: p.Target != nil}
	switch p.Phase {
	case "prepare", "pre_inflight", "pre_received", "put_inflight", "put_received", "finish_inflight", "finish_received", "metadata_confirmed":
		d.Phase = p.Phase
	}
	switch r.State {
	case "prepared", "submitting", "accepted", "unknown", "confirmed":
		d.State = r.State
	}
	if p.InFlightPart >= 0 && p.InFlightPart <= 10000 {
		d.InFlightPart = p.InFlightPart
	}
	if len(p.Parts) <= 10000 {
		d.ConfirmedParts = len(p.Parts)
	}
	if p.Observation != nil && p.Observation.Pages >= 0 && p.Observation.Pages <= 128 {
		d.TaskPages, d.TaskListingComplete = p.Observation.Pages, p.Observation.Complete
		if p.Observation.FinishStatus != nil && *p.Observation.FinishStatus >= 0 && *p.Observation.FinishStatus <= 2 {
			status := *p.Observation.FinishStatus
			d.TaskFinishStatus = &status
		}
		switch p.Observation.Reason {
		case "response_type_invalid", "marker_invalid", "parts_limit", "finish_status_invalid", "finish_status_regressed", "completed_fid_invalid", "completed_fid_changed", "part_invalid", "part_changed", "cursor_repeated", "page_limit", "original_part_unconfirmed", "task_unfinished":
			d.TaskQueryReason = p.Observation.Reason
		}
		switch p.Observation.Field {
		case "response", "finish_status", "fid", "uploaded_parts", "uploaded_parts.part_number", "uploaded_parts.etag", "etag", "next_part_number_marker":
			d.TaskResponseField = p.Observation.Field
		}
		switch p.Observation.ValueType {
		case "missing", "string", "object", "array", "boolean", "null", "number", "unknown":
			d.TaskResponseType = p.Observation.ValueType
		}
	}
	var unsent *requestNotDispatchedError
	var operation *OperationError
	if errors.As(err, &unsent) && errors.As(err, &operation) {
		// A blocked read only proves that query was not sent. It cannot
		// establish non-submission of the original unresolved upload write.
		switch operation.Stage {
		case "upload_prepare", "upload_part_prepare", "upload_finish":
			d.NotDispatched = true
		}
	}
	return &uploadStateError{diagnostic: d, cause: err}
}

func UploadDiagnosticFromError(err error) (UploadDiagnostic, bool) {
	var state *uploadStateError
	if !errors.As(err, &state) {
		return UploadDiagnostic{}, false
	}
	return state.diagnostic, true
}
