package pan115

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	sdk "github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

type ordinaryOfflineInput struct {
	Version                                     int `json:"version"`
	Scope, Action, Hash, URIDigest, DirectoryID string
	DeleteFiles, Remote                         bool
}
type ordinaryOfflineCheckpoint struct {
	Input             ordinaryOfflineInput
	Ack               bool
	DefiniteRejection bool
	Task              *cloud.OfflineTask `json:",omitempty"`
}
type ordinaryOfflineGuard struct {
	Store      cloud.ResourceOperationStore
	Receipt    cloud.OperationReceipt
	Checkpoint ordinaryOfflineCheckpoint
	Resource   string
}

func (c *Client) ordinaryStore() cloud.ResourceOperationStore {
	if c.selective == nil || c.selective.account == "" {
		return nil
	}
	store, _ := c.selective.store.(cloud.ResourceOperationStore)
	return store
}
func nativeV1Hash(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	decoded, err := hex.DecodeString(value)
	return value, len(value) == 40 && err == nil && len(decoded) == 20
}
func ordinaryURIHash(uri string) (string, error) {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(uri)), "magnet:") {
		return "", nil
	}
	magnet, err := resourcecontent.ParseMagnet(uri)
	if err != nil {
		return "", selectedError(cloud.CodeResponseInvalid)
	}
	return magnet.Identity.V1, nil
}
func offlineResourceKey(hash string) string { return selectedDigest([]string{"native-bt-v1", hash}) }

func (c *Client) checkOrdinaryOwner(ctx context.Context, hash, own string) error {
	store := c.ordinaryStore()
	hash, valid := nativeV1Hash(hash)
	if store == nil || !valid {
		return nil
	}
	r, err := store.ResourceClaim(ctx, offlineResourceKey(hash))
	if errors.Is(err, cloud.ErrOperationNotFound) {
		return nil
	}
	if err != nil {
		return selectedError(cloud.CodeUnavailable)
	}
	if r.Provider != cloud.ProviderPan115 || r.AccountID != c.selective.account || r.Route != "cookie" {
		return selectedError(cloud.CodeResponseInvalid)
	}
	if r.Kind != cloud.OfflineResourceGuardKind {
		return selectedError(cloud.CodeOfflineTaskExists)
	}
	if own != "" && own != r.ID {
		return selectedUnknown()
	}
	return nil
}

func (c *Client) prepareOrdinary(ctx context.Context, input ordinaryOfflineInput) (*ordinaryOfflineGuard, error) {
	store := c.ordinaryStore()
	if store == nil {
		return nil, selectedError(cloud.CodeUnavailable)
	}
	if input.Version != 1 || !selectedScalar(input.Scope) {
		return nil, selectedError(cloud.CodeResponseInvalid)
	}
	id := "omc-offline-" + selectedDigest([]string{input.Scope, input.Action})
	if err := c.checkOrdinaryOwner(ctx, input.Hash, id); err != nil {
		return nil, err
	}
	checkpoint := ordinaryOfflineCheckpoint{Input: input}
	r, err := store.Load(ctx, id)
	resource := offlineResourceKey(input.Hash)
	if errors.Is(err, cloud.ErrOperationNotFound) {
		r = selectedReceipt(c, id, cloud.OfflineResourceGuardKind, checkpoint)
		if err = store.Claim(ctx, resource, r); err != nil {
			r, err = store.Load(ctx, id)
		}
	}
	if err != nil {
		return nil, selectedUnknown()
	}
	var saved ordinaryOfflineCheckpoint
	if r.Provider != cloud.ProviderPan115 || r.AccountID != c.selective.account || r.Route != "cookie" || r.Kind != cloud.OfflineResourceGuardKind || json.Unmarshal(r.Payload, &saved) != nil || selectedDigest(saved.Input) != selectedDigest(input) {
		return nil, selectedError(cloud.CodeResponseInvalid)
	}
	if r.State == "failed" {
		rearmed := r
		rearmed.Revision++
		rearmed.State = "prepared"
		rearmed.UpdatedAt = time.Now().UTC()
		if err := store.ReclaimResourceClaim(ctx, resource, r.Revision, rearmed); err != nil {
			return nil, selectedUnknown()
		}
		r = rearmed
	}
	return &ordinaryOfflineGuard{store, r, saved, resource}, nil
}
func (g *ordinaryOfflineGuard) save(ctx context.Context, state string) error {
	r := g.Receipt
	r.Revision++
	r.State, r.UpdatedAt = state, time.Now().UTC()
	r.Payload, _ = json.Marshal(g.Checkpoint)
	if err := g.Store.Update(ctx, g.Receipt.Revision, r); err != nil {
		return selectedUnknown()
	}
	g.Receipt = r
	return nil
}
func (c *Client) releaseOrdinary(ctx context.Context, g *ordinaryOfflineGuard) error {
	if err := c.checkOrdinaryOwner(ctx, g.Checkpoint.Input.Hash, g.Receipt.ID); err != nil {
		return err
	}
	r := g.Receipt
	r.Revision++
	r.State, r.UpdatedAt = "confirmed", time.Now().UTC()
	r.Payload, _ = json.Marshal(g.Checkpoint)
	if err := g.Store.ReleaseResourceClaim(ctx, g.Resource, g.Receipt.Revision, r); err != nil {
		return selectedUnknown()
	}
	g.Receipt = r
	return nil
}

func (c *Client) failUndispatchedOrdinary(ctx context.Context, g *ordinaryOfflineGuard) {
	checkpointCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	r := g.Receipt
	r.Revision++
	r.State = "failed"
	r.UpdatedAt = time.Now().UTC()
	r.Payload, _ = json.Marshal(g.Checkpoint)
	_ = g.Store.ReleaseResourceClaim(checkpointCtx, g.Resource, g.Receipt.Revision, r)
}

func definiteOfflineRejection(err error) bool {
	return errors.Is(err, sdk.ErrNotLogin) || errors.Is(err, sdk.ErrBadCookie) || errors.Is(err, sdk.ErrOfflineNoTimes) || errors.Is(err, sdk.ErrOfflineInvalidLink)
}

func (c *Client) rejectOrdinary(ctx context.Context, g *ordinaryOfflineGuard) error {
	checkpointCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	g.Checkpoint.DefiniteRejection = true
	r := g.Receipt
	r.Revision++
	r.State = "failed"
	r.UpdatedAt = time.Now().UTC()
	r.Payload, _ = json.Marshal(g.Checkpoint)
	if err := g.Store.ReleaseResourceClaim(checkpointCtx, g.Resource, g.Receipt.Revision, r); err != nil {
		return selectedUnknown()
	}
	return nil
}
func ordinaryScope(ctx context.Context, fallback any) string {
	if scope := cloud.OfflineOperationScope(ctx); selectedScalar(scope) {
		return scope
	}
	return "legacy-" + selectedDigest(fallback)
}
func (c *Client) reconcileOrdinarySubmit(ctx context.Context, g *ordinaryOfflineGuard) (cloud.OfflineTask, error) {
	if err := c.checkOrdinaryOwner(ctx, g.Checkpoint.Input.Hash, g.Receipt.ID); err != nil {
		return cloud.OfflineTask{}, err
	}
	if g.Receipt.State == "confirmed" {
		return c.GetOffline(ctx, g.Checkpoint.Input.Hash)
	}
	if g.Receipt.State == "prepared" {
		return cloud.OfflineTask{}, selectedUnknown()
	}
	if g.Checkpoint.Ack {
		// A known SDK ACK only needs positive facts for this hash. Keep the
		// ordinary cached-page lookup instead of scanning every account page.
		task, err := c.getOfflineNative(ctx, g.Checkpoint.Input.Hash)
		if err != nil {
			return cloud.OfflineTask{}, selectedUnknown()
		}
		g.Checkpoint.Task = &task
		if err = c.releaseOrdinary(ctx, g); err != nil {
			return cloud.OfflineTask{}, err
		}
		return task, nil
	}
	native, found, err := c.selectedNativeTask(ctx, g.Checkpoint.Input.Hash, "")
	if err != nil {
		return cloud.OfflineTask{}, err
	}
	// Positive evidence in this exact task directory can reconcile a lost ACK.
	// Absence is never permission to release or dispatch a replacement.
	if !found || !g.Checkpoint.Ack && (g.Checkpoint.Input.DirectoryID == "" || native.DirId != g.Checkpoint.Input.DirectoryID) {
		return cloud.OfflineTask{}, selectedUnknown()
	}
	task := mapOfflineTask(native)
	g.Checkpoint.Ack, g.Checkpoint.Task = true, &task
	if err = c.releaseOrdinary(ctx, g); err != nil {
		return cloud.OfflineTask{}, err
	}
	return task, nil
}
func (c *Client) submitOrdinaryOffline(ctx context.Context, uri, directory, hash string) (cloud.OfflineTask, error) {
	release, err := c.lockOrdinaryResource(ctx, hash)
	if err != nil {
		return cloud.OfflineTask{}, err
	}
	defer release()
	input := ordinaryOfflineInput{Version: 1, Scope: ordinaryScope(ctx, []string{"submit", uri, directory}), Action: "submit", Hash: hash, URIDigest: selectedDigest(uri), DirectoryID: directory}
	g, err := c.prepareOrdinary(ctx, input)
	if err != nil {
		return cloud.OfflineTask{}, err
	}
	if g.Receipt.State != "prepared" {
		return c.reconcileOrdinarySubmit(ctx, g)
	}
	dispatched := false
	var hashes []string
	err = c.waitAndCall(ctx, c.offlineRate, func() error {
		if e := c.checkOrdinaryOwner(ctx, hash, g.Receipt.ID); e != nil {
			return e
		}
		g.Checkpoint.Ack, g.Checkpoint.DefiniteRejection, g.Checkpoint.Task = false, false, nil
		if e := g.save(ctx, "submitting"); e != nil {
			return e
		}
		dispatched = true
		var e error
		hashes, e = c.sdk.AddOfflineTaskURIs([]string{uri}, directory)
		return e
	})
	if !dispatched {
		c.failUndispatchedOrdinary(ctx, g)
		return cloud.OfflineTask{}, mapError(err)
	}
	if definiteOfflineRejection(err) {
		if releaseErr := c.rejectOrdinary(ctx, g); releaseErr != nil {
			return cloud.OfflineTask{}, releaseErr
		}
		return cloud.OfflineTask{}, mapError(err)
	}
	checkpointCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err == nil && len(hashes) == 1 {
		returned, ok := nativeV1Hash(hashes[0])
		g.Checkpoint.Ack = ok && returned == hash
	} else if errors.Is(err, sdk.ErrOfflineTaskExisted) {
		g.Checkpoint.Ack = true
	}
	if e := g.save(checkpointCtx, "unknown"); e != nil {
		return cloud.OfflineTask{}, e
	}
	return c.reconcileOrdinarySubmit(checkpointCtx, g)
}
func (c *Client) cancelOrdinaryOffline(ctx context.Context, hash string, deleteFiles bool) error {
	release, err := c.lockOrdinaryResource(ctx, hash)
	if err != nil {
		return err
	}
	defer release()
	action := "cancel"
	if deleteFiles {
		action = "cancel-data"
	}
	input := ordinaryOfflineInput{Version: 1, Scope: ordinaryScope(ctx, []string{action, hash}), Action: action, Hash: hash, DeleteFiles: deleteFiles}
	g, err := c.prepareOrdinary(ctx, input)
	if err != nil {
		return err
	}
	if g.Receipt.State == "confirmed" {
		return c.checkOrdinaryOwner(ctx, hash, "")
	}
	if g.Receipt.State != "prepared" {
		return c.reconcileOrdinaryCancel(ctx, g)
	}
	dispatched := false
	err = c.waitAndCall(ctx, c.offlineRate, func() error {
		if e := c.checkOrdinaryOwner(ctx, hash, g.Receipt.ID); e != nil {
			return e
		}
		g.Checkpoint.Ack, g.Checkpoint.DefiniteRejection, g.Checkpoint.Task = false, false, nil
		if e := g.save(ctx, "submitting"); e != nil {
			return e
		}
		dispatched = true
		return c.sdk.DeleteOfflineTasks([]string{hash}, deleteFiles)
	})
	if !dispatched {
		c.failUndispatchedOrdinary(ctx, g)
		return mapError(err)
	}
	if definiteOfflineRejection(err) {
		if releaseErr := c.rejectOrdinary(ctx, g); releaseErr != nil {
			return releaseErr
		}
		return mapError(err)
	}
	checkpointCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	g.Checkpoint.Ack = err == nil
	if e := g.save(checkpointCtx, "unknown"); e != nil {
		return e
	}
	return c.reconcileOrdinaryCancel(checkpointCtx, g)
}
func (c *Client) reconcileOrdinaryCancel(ctx context.Context, g *ordinaryOfflineGuard) error {
	if err := c.checkOrdinaryOwner(ctx, g.Checkpoint.Input.Hash, g.Receipt.ID); err != nil {
		return err
	}
	if !g.Checkpoint.Ack {
		return selectedUnknown()
	}
	_, found, err := c.selectedNativeTask(ctx, g.Checkpoint.Input.Hash, "")
	if err != nil {
		return err
	}
	if found {
		return selectedUnknown()
	}
	return c.releaseOrdinary(ctx, g)
}

func (c *Client) remoteOrdinaryInput(access cloud.OfflineAccess) (ordinaryOfflineInput, error) {
	hash, err := ordinaryURIHash(access.URI)
	if err != nil || hash == "" || !access.Remote || !selectedScalar(access.OperationID) {
		return ordinaryOfflineInput{}, selectedError(cloud.CodeResponseInvalid)
	}
	return ordinaryOfflineInput{Version: 1, Scope: access.OperationID, Action: "remote-submit", Hash: hash, URIDigest: selectedDigest(access.URI), DirectoryID: access.DirectoryID, Remote: true}, nil
}
func (c *Client) BeginOfflineAccess(ctx context.Context, access cloud.OfflineAccess) error {
	input, err := c.remoteOrdinaryInput(access)
	if err != nil {
		return err
	}
	g, err := c.prepareOrdinary(ctx, input)
	if err != nil {
		return err
	}
	if g.Receipt.State == "prepared" {
		return g.save(ctx, "submitting")
	}
	return nil
}

// The caller must have durably accepted the original Node operation as
// completed. Failed/cancelled/unknown Node operations never call this method.
func (c *Client) CompleteOfflineAccess(ctx context.Context, access cloud.OfflineAccess) error {
	input, err := c.remoteOrdinaryInput(access)
	if err != nil {
		return err
	}
	g, err := c.prepareOrdinary(ctx, input)
	if err != nil {
		return err
	}
	if g.Receipt.State == "confirmed" {
		return nil
	}
	// Check actual current native facts even after a durable Node completion.
	if c.selective.facts != nil {
		_, err = c.selective.facts.OfflineFacts(ctx, input.Hash)
		if code, _ := cloud.ErrorInfo(err); code == cloud.CodeNotFound {
			err = nil
		}
	} else if c.sdk != nil {
		_, _, err = c.selectedNativeTask(ctx, input.Hash, "")
	} else {
		err = selectedError(cloud.CodeUnavailable)
	}
	if err != nil {
		return err
	}
	g.Checkpoint.Ack = true
	return c.releaseOrdinary(ctx, g)
}

// NewOfflineAccessCoordinator performs no SDK request or credential probe.
// Completion requires a separate read-only provider evidence reader.
func NewOfflineAccessCoordinator(store cloud.ResourceOperationStore, account string, facts cloud.OfflineFactReader) cloud.OfflineAccessGuard {
	return &Client{selective: &selectiveOfflineState{store: store, account: account, facts: facts}}
}

func (c *Client) OfflineFacts(ctx context.Context, hash string) (cloud.OfflineTask, error) {
	if _, ok := nativeV1Hash(hash); !ok {
		return cloud.OfflineTask{}, selectedError(cloud.CodeResponseInvalid)
	}
	return c.getOfflineNative(ctx, hash)
}

var _ cloud.OfflineAccessGuard = (*Client)(nil)

type ordinaryResourceLock struct {
	token chan struct{}
	refs  int
}

var ordinaryResourceLocks = struct {
	sync.Mutex
	entries map[string]*ordinaryResourceLock
}{entries: map[string]*ordinaryResourceLock{}}

// The durable claim survives process loss. This additional physical-call lock
// prevents a concurrent same-operation observer from publishing completion and
// releasing that claim while the original SDK call is still executing here.
func (c *Client) lockOrdinaryResource(ctx context.Context, hash string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, cloud.Error(cloud.CodeUnavailable, true, err)
	}
	key := selectedDigest([]string{c.selective.account, hash})
	ordinaryResourceLocks.Lock()
	entry := ordinaryResourceLocks.entries[key]
	if entry == nil {
		entry = &ordinaryResourceLock{token: make(chan struct{}, 1)}
		ordinaryResourceLocks.entries[key] = entry
	}
	entry.refs++
	ordinaryResourceLocks.Unlock()
	drop := func() {
		ordinaryResourceLocks.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(ordinaryResourceLocks.entries, key)
		}
		ordinaryResourceLocks.Unlock()
	}
	select {
	case entry.token <- struct{}{}:
		return func() { <-entry.token; drop() }, nil
	case <-ctx.Done():
		drop()
		return nil, cloud.Error(cloud.CodeUnavailable, true, ctx.Err())
	}
}
