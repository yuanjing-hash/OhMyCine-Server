package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"

	cloud "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

// Catalog and transfer manifests always retain TV identities. Management IDs
// are resolved independently under the connection's verified common roots.
type quarkTransferBackend interface {
	cloud.MutationDriver
	cloud.UploadDriver
	cloud.ReadDriver
	ResolveManagementItem(context.Context, string) (cloud.Item, error)
	ResolveReadItem(context.Context, string) (cloud.Item, error)
}

type quarkTransferDriver struct {
	cloud.Driver
	backend         quarkTransferBackend
	taskID          string
	verifiedUploads map[string]nodeprotocol.ContentDigest
}

func transferCloudDriver(driver cloud.Driver, taskID string) (cloud.Driver, error) {
	if driver.Provider() != cloud.ProviderQuark {
		return driver, nil
	}
	backend, ok := driver.(quarkTransferBackend)
	if !ok || taskID == "" {
		return nil, cloudTransferError("quark_transfer_capability_missing", false, nil)
	}
	return &quarkTransferDriver{Driver: driver, backend: backend, taskID: taskID, verifiedUploads: map[string]nodeprotocol.ContentDigest{}}, nil
}

func (d *quarkTransferDriver) operation(ctx context.Context, kind string, parts ...string) context.Context {
	return quark.WithOperationID(ctx, quarkTransferOperationID(d.taskID, kind, parts...))
}

func quarkTransferOperationID(taskID, kind string, parts ...string) string {
	identity, _ := json.Marshal(append([]string{taskID, kind}, parts...))
	sum := sha256.Sum256(identity)
	return "transfer-" + hex.EncodeToString(sum[:])
}

func (d *quarkTransferDriver) CreateDirectory(ctx context.Context, parent, name string) (cloud.Item, error) {
	p, err := d.backend.ResolveManagementItem(ctx, parent)
	if err != nil {
		return cloud.Item{}, err
	}
	opCtx := d.operation(ctx, "mkdir", parent, name)
	created, err := d.backend.CreateDirectory(opCtx, p.ID, name)
	if err != nil {
		return cloud.Item{}, err
	}
	item, err := d.backend.ResolveReadItem(opCtx, created.ID)
	if err != nil {
		return cloud.Item{}, err
	}
	if !item.IsDir || item.ParentID != parent || item.Name != name {
		return cloud.Item{}, cloudTransferError("quark_transfer_identity_mismatch", false, nil)
	}
	return item, nil
}

func (d *quarkTransferDriver) Move(ctx context.Context, id, parent string) error {
	item, err := d.backend.ResolveManagementItem(ctx, id)
	if err != nil {
		return err
	}
	p, err := d.backend.ResolveManagementItem(ctx, parent)
	if err != nil {
		return err
	}
	if err = d.backend.Move(d.operation(ctx, "move", id, parent), item.ID, p.ID); err != nil {
		return err
	}
	verified, err := d.backend.ResolveReadItem(ctx, item.ID)
	if err != nil {
		return err
	}
	if verified.ID != id || verified.ParentID != parent {
		return cloudTransferError("quark_transfer_identity_mismatch", false, nil)
	}
	return nil
}

func (d *quarkTransferDriver) Rename(ctx context.Context, id, name string) error {
	item, err := d.backend.ResolveManagementItem(ctx, id)
	if err != nil {
		return err
	}
	if err = d.backend.Rename(d.operation(ctx, "rename", id, name), item.ID, name); err != nil {
		return err
	}
	verified, err := d.backend.ResolveReadItem(ctx, item.ID)
	if err != nil {
		return err
	}
	if verified.ID != id || verified.Name != name {
		return cloudTransferError("quark_transfer_identity_mismatch", false, nil)
	}
	return nil
}

func (d *quarkTransferDriver) Copy(context.Context, string, string) error {
	return cloudTransferError("quark_copy_unavailable", false, nil)
}
func (d *quarkTransferDriver) Recycle(context.Context, string) error {
	return cloudTransferError("quark_recycle_unavailable", false, nil)
}

func (d *quarkTransferDriver) Upload(ctx context.Context, request cloud.UploadRequest) (cloud.Item, error) {
	if request.Reader == nil || request.Size < 0 || request.Size == math.MaxInt64 {
		return cloud.Item{}, cloudTransferError("cloud_upload_source_changed", false, nil)
	}
	parent, err := d.backend.ResolveManagementItem(ctx, request.ParentID)
	if err != nil {
		return cloud.Item{}, err
	}
	if _, err = request.Reader.Seek(0, io.SeekStart); err != nil {
		return cloud.Item{}, err
	}
	digest := sha256.New()
	n, err := io.Copy(digest, io.LimitReader(request.Reader, request.Size+1))
	if err != nil || n != request.Size {
		return cloud.Item{}, cloudTransferError("cloud_upload_source_changed", false, err)
	}
	expected := hex.EncodeToString(digest.Sum(nil))
	if _, err = request.Reader.Seek(0, io.SeekStart); err != nil {
		return cloud.Item{}, err
	}
	opCtx := d.operation(ctx, "upload", request.ParentID, request.Name)
	agentRequest := request
	agentRequest.ParentID = parent.ID
	output, err := d.backend.Upload(opCtx, agentRequest)
	if err != nil {
		return cloud.Item{}, err
	}
	var verified cloud.Item
	if resolver, ok := d.backend.(interface {
		ResolveUploadReadbackItem(context.Context, string) (cloud.Item, error)
	}); ok {
		// The native resolver only removes a duplicate mapping read. Its
		// metadata candidate is never sufficient to complete this upload.
		verified, err = resolver.ResolveUploadReadbackItem(opCtx, output.ID)
	} else {
		verified, err = d.backend.ResolveReadItem(opCtx, output.ID)
	}
	if err != nil {
		return cloud.Item{}, err
	}
	if verified.IsDir || verified.ParentID != request.ParentID || verified.Name != request.Name || verified.Size != request.Size {
		return cloud.Item{}, cloudTransferError("quark_transfer_identity_mismatch", false, nil)
	}
	// Agent upload acknowledgements do not prove content equality. A complete
	// original-file TV readback must match before the task may publish or clean up.
	stream, err := d.backend.OpenRead(ctx, cloud.ReadRequest{FileID: verified.ID})
	if err != nil {
		return cloud.Item{}, err
	}
	if stream.Body == nil {
		return cloud.Item{}, cloudTransferError("cloud_upload_readback_failed", true, nil)
	}
	defer stream.Body.Close()
	if stream.TotalSize != nil && *stream.TotalSize != request.Size {
		return cloud.Item{}, cloudTransferError("cloud_upload_readback_mismatch", false, nil)
	}
	digest.Reset()
	n, err = io.Copy(digest, io.LimitReader(stream.Body, request.Size+1))
	if err != nil {
		return cloud.Item{}, cloudTransferError("cloud_upload_readback_failed", true, err)
	}
	if n != request.Size || hex.EncodeToString(digest.Sum(nil)) != expected {
		return cloud.Item{}, cloudTransferError("cloud_upload_readback_mismatch", false, nil)
	}
	d.verifiedUploads[verified.ID] = nodeprotocol.ContentDigest{Version: 2, Algorithm: "sha256", Value: expected, VerifiedBy: "full_readback"}
	return verified, nil
}

func cloudTransferConflictOptions(canRecycle bool) []string {
	options := []string{"skip", "rename"}
	if canRecycle {
		options = append([]string{"overwrite"}, options...)
	}
	return options
}
