package quark

import (
	"errors"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// UploadDiagnostic is a bounded projection, never the private upload payload.
// It deliberately contains no task/receipt/file ID, name, URL, hash or ETag.
type UploadDiagnostic struct {
	Phase, State                                      string
	HasTask, HasOutput, TargetRecorded, NotDispatched bool
	InFlightPart, ConfirmedParts                      int
	TaskFinishStatus                                  *int
	TaskPages                                         int
	TaskListingComplete                               bool
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
	}
	var unsent *requestNotDispatchedError
	d.NotDispatched = errors.As(err, &unsent)
	return &uploadStateError{diagnostic: d, cause: err}
}

func UploadDiagnosticFromError(err error) (UploadDiagnostic, bool) {
	var state *uploadStateError
	if !errors.As(err, &state) {
		return UploadDiagnostic{}, false
	}
	return state.diagnostic, true
}
