package nodeagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

type nodeQuarkUploadDriver struct {
	*quark.Client
	operationKey string
	guard        func(context.Context) error
}

func validateQuarkGrant(c nodeprotocol.StorageCredential) (quark.Credentials, error) {
	b, e := quark.ParseCredentials(c.Quark)
	if e != nil || c.ProviderType != nodeprotocol.StorageProviderQuark || c.Cookie != "" || c.RootID == "" || b.TV == nil || b.Agent == nil || b.Web != nil || b.TV.RefreshToken != "" || b.Agent.RefreshToken != "" || len(b.Proofs) != 1 || !b.HasProof(c.RootID, b.Proofs[0].AgentRoot) {
		return quark.Credentials{}, errors.New("node_credential_payload_invalid")
	}
	return b, nil
}

func configureQuarkDirectoryLocator(client *quark.Client, c nodeprotocol.StorageCredential, relative, expectedID string) error {
	bundle, err := validateQuarkGrant(c)
	if err != nil {
		return err
	}
	if relative == "" && expectedID == c.RootID {
		relative = "/"
	}
	if !nodeprotocol.ValidStorageDirectoryRelativePath(relative) {
		return errors.New(nodeprotocol.ErrorPlanConflict)
	}
	return client.SetDirectoryLocator(c.RootID, bundle.Proofs[0].Path, relative, expectedID)
}
func (a *Agent) newQuarkUploadDriver(ctx context.Context, serverID string, input nodeprotocol.StorageActionRequest, c nodeprotocol.StorageCredential, grant *quarkGrantState) (storageUploadDriver, error) {
	b, e := validateQuarkGrant(c)
	if e != nil {
		return nil, e
	}
	receipts, e := a.quarkReceipts(serverID, input.OperationKey)
	if e != nil {
		return nil, e
	}
	factory := a.quarkClientFactory
	if factory == nil {
		factory = quark.NewClient
	}
	client, e := factory(cloud.Config{Credentials: c.Quark, Operations: receipts, BeforeCall: grant.validateCurrent})
	if e != nil {
		return nil, e
	}
	if e = client.SetRestrictedCredentialLoader(grant.load); e != nil {
		return nil, e
	}
	root, e := client.ResolveDirectory(ctx, b.Proofs[0].Path)
	if e != nil || root.ID != c.RootID || !root.IsDir {
		return nil, errors.New(nodeprotocol.ErrorPlanConflict)
	}
	if _, e = client.ResolveManagementItem(ctx, root.ID); e != nil {
		return nil, e
	}
	return &nodeQuarkUploadDriver{client, input.OperationKey, grant.check}, nil
}

type quarkGuardedBody struct {
	io.ReadCloser
	ctx   context.Context
	guard func(context.Context) error
	stop  func()
	once  sync.Once
}

func (r *quarkGuardedBody) Read(p []byte) (int, error) {
	if e := r.guard(r.ctx); e != nil {
		return 0, e
	}
	n, err := r.ReadCloser.Read(p)
	if cause := context.Cause(r.ctx); cause != nil {
		return n, cause
	}
	if err == nil {
		err = r.guard(r.ctx)
	}
	return n, err
}
func (r *quarkGuardedBody) Close() error {
	if r.stop != nil {
		r.once.Do(r.stop)
	}
	return r.ReadCloser.Close()
}

// A body may block without another Read call. Check the same durable operation
// and short grant while I/O is pending, and cancel the transport's context when
// either is revoked. Stop waits for the watcher; no background worker survives
// the owned stream/Upload. No credentials or provider refresh run here.
func quarkGuardedContext(ctx context.Context, guard func(context.Context) error) (context.Context, func(), error) {
	if err := guard(ctx); err != nil {
		return nil, nil, err
	}
	guarded, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-guarded.Done():
				return
			case <-ticker.C:
				if err := guard(guarded); err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	return guarded, func() { cancel(context.Canceled); <-done }, nil
}
func (d *nodeQuarkUploadDriver) OpenRead(ctx context.Context, r cloud.ReadRequest) (cloud.ReadResult, error) {
	if _, e := d.scopedID(ctx, r.FileID); e != nil {
		return cloud.ReadResult{}, e
	}
	guarded, stop, e := quarkGuardedContext(ctx, d.guard)
	if e != nil {
		return cloud.ReadResult{}, e
	}
	out, e := d.Client.OpenRead(guarded, r)
	if e == nil && out.Body != nil {
		out.Body = &quarkGuardedBody{ReadCloser: out.Body, ctx: guarded, guard: d.guard, stop: stop}
	} else {
		if cause := context.Cause(guarded); cause != nil {
			e = cause
		}
		stop()
	}
	return out, e
}
func (d *nodeQuarkUploadDriver) scopedID(ctx context.Context, id string) (cloud.Item, error) {
	return d.ResolveManagementItem(ctx, id)
}
func (d *nodeQuarkUploadDriver) Stat(ctx context.Context, id string) (cloud.Item, error) {
	if _, e := d.scopedID(ctx, id); e != nil {
		return cloud.Item{}, e
	}
	return d.Client.Stat(ctx, id)
}
func (d *nodeQuarkUploadDriver) List(ctx context.Context, id string, p cloud.PageRequest) (cloud.Page, error) {
	if _, e := d.scopedID(ctx, id); e != nil {
		return cloud.Page{}, e
	}
	return d.Client.List(ctx, id, p)
}
func nodeQuarkOperationID(operation, kind, parent, name string) string {
	h := sha256.Sum256([]byte(operation + "\x00" + kind + "\x00" + parent + "\x00" + name))
	return "node-" + kind + "-" + hex.EncodeToString(h[:])
}
func (d *nodeQuarkUploadDriver) CreateDirectory(ctx context.Context, parent, name string) (cloud.Item, error) {
	p, e := d.scopedID(ctx, parent)
	if e != nil {
		return cloud.Item{}, e
	}
	opCtx := quark.WithOperationID(ctx, nodeQuarkOperationID(d.operationKey, "mkdir", parent, name))
	item, e := d.Client.CreateDirectory(opCtx, p.ID, name)
	if e != nil {
		return cloud.Item{}, e
	}
	return d.ResolveReadItem(opCtx, item.ID)
}
func (d *nodeQuarkUploadDriver) Upload(ctx context.Context, r cloud.UploadRequest) (cloud.Item, error) {
	guarded, stop, e := quarkGuardedContext(ctx, d.guard)
	if e != nil {
		return cloud.Item{}, e
	}
	defer stop()
	ctx = guarded
	p, e := d.scopedID(ctx, r.ParentID)
	if e != nil {
		return cloud.Item{}, e
	}
	id := nodeQuarkOperationID(d.operationKey, "upload", r.ParentID, r.Name)
	opCtx := quark.WithOperationID(ctx, id)
	r.ParentID = p.ID
	item, e := d.Client.Upload(opCtx, r)
	if e != nil {
		if cause := context.Cause(ctx); cause != nil {
			e = cause
		}
		return cloud.Item{}, e
	}
	// Only resolves the original upload's metadata candidate. The storage
	// worker checkpoints it, then always runs verifyQuarkReadback, including
	// before/after identity, revision and unique-location checks.
	return d.ResolveUploadReadbackItem(opCtx, item.ID)
}

func verifyQuarkReadback(ctx context.Context, driver storageUploadDriver, item cloud.Item, parent, name string, size int64, expected string) (cloud.Item, error) {
	reader, ok := driver.(cloud.ReadDriver)
	if !ok {
		return cloud.Item{}, errors.New(nodeprotocol.ErrorCapabilityMissing)
	}
	before, e := driver.Stat(ctx, item.ID)
	if e != nil {
		return cloud.Item{}, e
	}
	if before.IsDir || before.ParentID != parent || before.Name != name || before.Size != size {
		return cloud.Item{}, errors.New(nodeprotocol.ErrorChecksumMismatch)
	}
	stream, e := reader.OpenRead(ctx, cloud.ReadRequest{FileID: before.ID})
	if e != nil {
		return cloud.Item{}, e
	}
	if stream.Body == nil {
		return cloud.Item{}, errors.New(nodeprotocol.ErrorChecksumMismatch)
	}
	defer stream.Body.Close()
	if stream.TotalSize == nil || *stream.TotalSize != size {
		return cloud.Item{}, errors.New(nodeprotocol.ErrorChecksumMismatch)
	}
	hash := sha256.New()
	n, e := io.Copy(hash, &contextReader{ctx: ctx, reader: io.LimitReader(stream.Body, size+1)})
	if e != nil {
		return cloud.Item{}, e
	}
	if n != size || hex.EncodeToString(hash.Sum(nil)) != expected {
		return cloud.Item{}, errors.New(nodeprotocol.ErrorChecksumMismatch)
	}
	after, e := driver.Stat(ctx, before.ID)
	if e != nil {
		return cloud.Item{}, e
	}
	if after.ID != before.ID || after.ParentID != parent || after.Name != name || after.Size != size || after.IsDir || after.Revision != before.Revision {
		return cloud.Item{}, errors.New(nodeprotocol.ErrorReconciliationNeeded)
	}
	matches, e := listStorageName(ctx, driver, parent, name)
	if e != nil {
		return cloud.Item{}, e
	}
	if len(matches) != 1 || matches[0].ID != before.ID {
		return cloud.Item{}, errors.New(nodeprotocol.ErrorReconciliationNeeded)
	}
	return after, nil
}

func (a *Agent) uploadQuarkStorageFile(ctx context.Context, serverID string, input nodeprotocol.StorageActionRequest, plan nodeprotocol.StorageUploadPlan, f nodeprotocol.StorageUploadFile, driver storageUploadDriver, guard func(context.Context) error) (nodeprotocol.StorageUploadFileResult, error) {
	out := nodeprotocol.StorageUploadFileResult{SourceFileToken: f.SourceFileToken, TargetName: path.Base(f.TargetRelativePath), Size: f.Size, Status: nodeprotocol.StorageFilePending}
	checkpoint, e := a.store.PrepareStorageUploadFile(ctx, input.OperationKey, f.SourceFileToken, input.PlanDigest, f.Size, a.now())
	if e != nil {
		return out, e
	}
	source, e := a.store.FileExportRangeFile(ctx, serverID, plan.SourceExportOperationKey, input.TaskID, f.SourceFileToken, a.now())
	if e != nil || source.Size != f.Size || source.SHA256 != f.SHA256 {
		return out, ErrPlanConflict
	}
	handle, e := a.openManagedExportFile(source)
	if e != nil {
		return out, errors.New(nodeprotocol.ErrorChecksumMismatch)
	}
	defer handle.Close()
	if _, e = hashStorageSource(ctx, handle, f.Size, f.SHA256); e != nil {
		return out, e
	}
	parent, e := ensureStorageParent(ctx, driver, plan.TargetRootID, path.Dir(f.TargetRelativePath), guard)
	if e != nil {
		return out, e
	}
	out.TargetParentID = parent
	matches, e := listStorageName(ctx, driver, parent, out.TargetName)
	if e != nil {
		return out, e
	}
	save := func(item cloud.Item, status string) (nodeprotocol.StorageUploadFileResult, error) {
		verified, e := verifyQuarkReadback(ctx, driver, item, parent, out.TargetName, f.Size, f.SHA256)
		if e != nil {
			return out, e
		}
		if e = a.store.UpdateStorageUploadFile(ctx, input.OperationKey, f.SourceFileToken, input.PlanDigest, status, parent, verified.ID, "", a.now()); e != nil {
			return out, e
		}
		out.TargetItemID = verified.ID
		out.Status = status
		out.ContentDigest = &nodeprotocol.ContentDigest{Version: 2, Algorithm: "sha256", Value: f.SHA256, VerifiedBy: "full_readback"}
		return out, nil
	}
	if checkpoint.Status == nodeprotocol.StorageFileSkipped {
		out.Status = nodeprotocol.StorageFileSkipped
		return out, nil
	}
	if checkpoint.TargetItemID != "" {
		if len(matches) != 1 || matches[0].ID != checkpoint.TargetItemID {
			return out, errors.New(nodeprotocol.ErrorReconciliationNeeded)
		}
		status := nodeprotocol.StorageFileCompleted
		if checkpoint.Status == nodeprotocol.StorageFileReused {
			status = checkpoint.Status
		}
		return save(matches[0], status)
	}
	// Accepted or uncertain writes recover the provider's durable receipt. They
	// must never be inferred successful just because a same-named file exists.
	recovering := checkpoint.Status == nodeprotocol.StorageFileUploading || checkpoint.Status == nodeprotocol.StorageFileReconciling
	if !recovering && len(matches) > 0 {
		switch f.ConflictAction {
		case nodeprotocol.StorageConflictSkipIfExists:
			out.Status = nodeprotocol.StorageFileSkipped
			e = a.store.UpdateStorageUploadFile(ctx, input.OperationKey, f.SourceFileToken, input.PlanDigest, out.Status, parent, "", "", a.now())
			return out, e
		case nodeprotocol.StorageConflictReuseIfIdentical:
			if len(matches) == 1 {
				return save(matches[0], nodeprotocol.StorageFileReused)
			}
		}
		return out, ErrPlanConflict
	}
	if e = guard(ctx); e != nil {
		return out, e
	}
	if e = a.store.UpdateStorageUploadFile(ctx, input.OperationKey, f.SourceFileToken, input.PlanDigest, nodeprotocol.StorageFileUploading, parent, "", "", a.now()); e != nil {
		return out, e
	}
	if _, e = handle.Seek(0, io.SeekStart); e != nil {
		return out, e
	}
	item, e := driver.Upload(ctx, cloud.UploadRequest{ParentID: parent, Name: out.TargetName, Size: f.Size, Reader: handle})
	if e != nil {
		_ = a.store.UpdateStorageUploadFile(ctx, input.OperationKey, f.SourceFileToken, input.PlanDigest, nodeprotocol.StorageFileReconciling, parent, "", "", a.now())
		return out, e
	}
	// Persist exact output identity before expensive verification. A restart
	// repeats readback, never upload and never source cleanup without proof.
	if e = a.store.UpdateStorageUploadFile(ctx, input.OperationKey, f.SourceFileToken, input.PlanDigest, nodeprotocol.StorageFileReconciling, parent, item.ID, "", a.now()); e != nil {
		return out, e
	}
	return save(item, nodeprotocol.StorageFileCompleted)
}
