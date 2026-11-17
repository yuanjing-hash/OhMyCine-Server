package downloader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"path"
	"sort"
	"strings"
	"time"
)

type OfflineOperationKind string

const OfflineOperationCloud OfflineOperationKind = "cloud_offline"
const (
	OfflineStartDispatch           = "dispatch"
	OfflineStartAccepted           = "accepted"
	OfflineStartProviderAdded      = "provider_added"
	OfflineCompletionUnknown       = "unknown"
	OfflineCompletionAggregate     = "aggregate"
	OfflineCompletionRequiredFiles = "required_files"
)

// OfflineOperation describes the actual frozen source, rather than a provider
// name. AccountNamespace is private verified factory authority; an empty value
// never permits account-wide history or an ownership fallback.
type OfflineOperation struct {
	Kind                   OfflineOperationKind
	AccountNamespace       string
	KeepOutputStop         bool
	RequiredFileCompletion bool
}
type OfflineLifecycleClient interface{ OfflineOperation(Source) OfflineOperation }

func ResolveOfflineOperation(client Client, source Source) OfflineOperation {
	if lifecycle, ok := client.(OfflineLifecycleClient); ok {
		return lifecycle.OfflineOperation(source)
	}
	return OfflineOperation{}
}

// OfflineObservation is a cloud-phase fact. Ready may precede Task.Completed
// when a remote executor is still transporting verified output. Manifest.Complete
// alone is enumeration, and must never be promoted to this observation.
type OfflineObservation struct {
	TerminalFailure       bool                 `json:"terminal_failure,omitempty"`
	DefiniteNonAcceptance bool                 `json:"definite_non_acceptance,omitempty"`
	DeadlineExceeded      bool                 `json:"deadline_exceeded,omitempty"`
	StopPending           bool                 `json:"stop_pending,omitempty"`
	UpstreamStopped       bool                 `json:"upstream_stopped,omitempty"`
	Version               int                  `json:"version"`
	Kind                  OfflineOperationKind `json:"kind"`
	OperationID           string               `json:"operation_id,omitempty"`
	AccountNamespace      string               `json:"account_namespace,omitempty"`
	OutputRootID          string               `json:"output_root_id,omitempty"`
	StartedAt             *time.Time           `json:"started_at,omitempty"`
	StartProvenance       string               `json:"start_provenance,omitempty"`
	Ready                 bool                 `json:"ready"`
	ReadyAt               *time.Time           `json:"ready_at,omitempty"`
	CompletionMode        string               `json:"completion_mode"`
	RequiredDigest        string               `json:"required_digest,omitempty"`
	ReadyManifest         *Manifest            `json:"ready_manifest,omitempty"`
	KeepOutputStop        bool                 `json:"keep_output_stop"`
}

// OfflineRequiredPlan is separate from an explicit executable selection. It
// cannot change a previously accepted wanted set or authorize broad cleanup.
type OfflineRequiredPlan struct {
	Version   int                   `json:"version"`
	RootName  string                `json:"root_name"`
	MultiFile bool                  `json:"multi_file"`
	Files     []OfflineRequiredFile `json:"files"`
}
type OfflineRequiredFile struct {
	RelativePath string `json:"relative_path"`
	Size         int64  `json:"size"`
}

func (p OfflineRequiredPlan) Digest() (string, error) {
	if p.Version != 1 || strings.TrimSpace(p.RootName) == "" || len(p.RootName) > 1024 || p.RootName == "." || p.RootName == ".." || strings.ContainsAny(p.RootName, "/\\\x00\r\n") || len(p.Files) == 0 || len(p.Files) > 1000 {
		return "", Error("downloader_offline_required_invalid", false, nil)
	}
	copy := p
	copy.Files = append([]OfflineRequiredFile(nil), p.Files...)
	sort.Slice(copy.Files, func(i, j int) bool { return copy.Files[i].RelativePath < copy.Files[j].RelativePath })
	seen := map[string]bool{}
	for _, f := range copy.Files {
		key := strings.ToLower(f.RelativePath)
		if f.Size < 0 || f.RelativePath == "" || len(f.RelativePath) > 4096 || strings.ContainsAny(f.RelativePath, "\\:\x00\r\n") || path.IsAbs(f.RelativePath) || path.Clean(f.RelativePath) != f.RelativePath || f.RelativePath == "." || strings.HasPrefix(f.RelativePath, "../") || seen[key] {
			return "", Error("downloader_offline_required_invalid", false, nil)
		}
		seen[key] = true
	}
	raw, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

type OfflineRequiredBinder interface {
	BindOfflineRequired(OfflineRequiredPlan) error
}

func BindOfflineRequired(client Client, plan *OfflineRequiredPlan) error {
	if plan == nil {
		return nil
	}
	if _, err := plan.Digest(); err != nil {
		return err
	}
	if binder, ok := client.(OfflineRequiredBinder); ok {
		return binder.BindOfflineRequired(*plan)
	}
	return nil // optional proof; unsupported executors retain aggregate completion
}

// Forward through immutable wrappers explicitly; embedding Client alone loses
// these optional interfaces. Task observations themselves need no forwarding.
func (c *selectionClient) OfflineOperation(s Source) OfflineOperation {
	return ResolveOfflineOperation(c.Client, s)
}
func (c *selectionClient) BindOfflineRequired(p OfflineRequiredPlan) error {
	return BindOfflineRequired(c.Client, &p)
}
func (c *verifiedAllClient) OfflineOperation(s Source) OfflineOperation {
	return ResolveOfflineOperation(c.Client, s)
}
func (c *verifiedAllClient) BindOfflineRequired(p OfflineRequiredPlan) error {
	return BindOfflineRequired(c.Client, &p)
}

// OfflineStopper stops only the owned cloud operation; remote data transport is
// independent and may continue from already proven intended output.
type OfflineStopper interface {
	StopOfflineKeepOutput(context.Context, string) error
}

func StopOfflineKeepOutput(ctx context.Context, client Client, id string) error {
	if stopper, ok := client.(OfflineStopper); ok {
		return stopper.StopOfflineKeepOutput(ctx, id)
	}
	return client.Cancel(ctx, id, false)
}
func (c *selectionClient) StopOfflineKeepOutput(ctx context.Context, id string) error {
	return c.selected.CancelSelected(ctx, id, c.binding, false)
}
func (c *verifiedAllClient) StopOfflineKeepOutput(ctx context.Context, id string) error {
	return StopOfflineKeepOutput(ctx, c.Client, id)
}

// OfflineObserver is guaranteed read-only with respect to native submission
// and remote materialization. Deadline reconciliation uses this narrow path.
type OfflineObserver interface {
	ObserveOffline(context.Context, string) (Task, error)
}

func ObserveOffline(ctx context.Context, client Client, id string) (Task, error) {
	if observer, ok := client.(OfflineObserver); ok {
		return observer.ObserveOffline(ctx, id)
	}
	return Task{}, Error("downloader_offline_observation_unsupported", true, nil)
}
func (c *selectionClient) ObserveOffline(ctx context.Context, id string) (Task, error) {
	return c.selected.GetSelected(ctx, id, c.binding)
}
func (c *verifiedAllClient) ObserveOffline(ctx context.Context, id string) (Task, error) {
	return ObserveOffline(ctx, c.Client, id)
}

func DefiniteSubmissionNonAcceptance(err error) bool {
	var proof interface{ SubmissionNotAccepted() bool }
	return err != nil && errors.As(err, &proof) && proof.SubmissionNotAccepted()
}
func WithDefiniteSubmissionNonAcceptance(err error) error {
	return cloud.WithDefiniteSubmissionNonAcceptance(err)
}

type terminalOfflineFailure struct{ error }

func (e *terminalOfflineFailure) OfflineOperationSettled() bool { return true }
func (e *terminalOfflineFailure) Unwrap() error                 { return e.error }
func WithOfflineTerminalFailure(err error) error {
	if err == nil {
		return nil
	}
	return &terminalOfflineFailure{err}
}
func OfflineTerminalFailure(err error) bool {
	var proof interface{ OfflineOperationSettled() bool }
	return err != nil && errors.As(err, &proof) && proof.OfflineOperationSettled()
}
