package nodeagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

func (a *Agent) runQuarkStorageSource(ctx context.Context, serverID string, input nodeprotocol.StorageSourceActionRequest, plan nodeprotocol.StorageSourcePlan, grant nodeprotocol.CredentialGrant, credential nodeprotocol.StorageSourceCredential) {
	run, e := a.store.StorageSourceRun(ctx, serverID, input.OperationKey)
	if e != nil {
		return
	}
	response := sourceResponse(input, run)
	response.CredentialExpiresAt = &grant.ExpiresAt
	fail := func(e error) { a.pauseStorageSource(ctx, serverID, input, response, e) }
	if !containsAction(grant.AllowedActions, nodeprotocol.StorageSourceGrantRead) {
		fail(errors.New("node_credential_binding_mismatch"))
		return
	}
	state := a.newQuarkGrantState(serverID, input.PlanDigest, grant, nodeprotocol.StorageCredential{ProviderType: plan.ProviderType, RootID: credential.RootID, Quark: credential.Quark})
	guard := state.check
	base, e := a.newQuarkUploadDriver(ctx, serverID, nodeprotocol.StorageActionRequest{OperationKey: input.OperationKey}, nodeprotocol.StorageCredential{ProviderType: plan.ProviderType, Quark: credential.Quark, RootID: credential.RootID}, state)
	if e != nil {
		fail(e)
		return
	}
	driver := base.(*nodeQuarkUploadDriver)
	storageCredential := nodeprotocol.StorageCredential{ProviderType: plan.ProviderType, Quark: credential.Quark, RootID: credential.RootID}
	outputRelative := path.Join("/", plan.DestinationRelativePath, plan.TaskDirectoryName)
	if e = configureQuarkDirectoryLocator(driver.Client, storageCredential, plan.DestinationRelativePath, plan.OfflineDestinationID); e != nil {
		fail(e)
		return
	}
	if !run.ManifestReady {
		id := nodeQuarkOperationID(input.OperationKey, "share", plan.OfflineDestinationID, plan.TaskDirectoryName)
		var selected quark.SelectionResult
		if run.OutputRootID == "" {
			if !containsAction(grant.AllowedActions, nodeprotocol.StorageSourceGrantShareInspect) || !containsAction(grant.AllowedActions, nodeprotocol.StorageSourceGrantShareReceive) {
				fail(errors.New("node_credential_binding_mismatch"))
				return
			}
			src, err := cloud.DecodeSelectedShareSource(credential.SourceURI)
			if err != nil {
				fail(err)
				return
			}
			selected, e = driver.ReceiveSelection(ctx, id, src.URL, plan.OfflineDestinationID, plan.TaskDirectoryName, &src.Selection)
		} else {
			selected, e = driver.GetSelection(ctx, id)
		}
		if e != nil {
			fail(e)
			return
		}
		if !selected.Complete || !selected.Root.IsDir || selected.Root.ParentID != plan.OfflineDestinationID || selected.Root.Name != plan.TaskDirectoryName || run.OutputRootID != "" && selected.Root.ID != run.OutputRootID {
			fail(errors.New(nodeprotocol.ErrorReconciliationNeeded))
			return
		}
		if e = configureQuarkDirectoryLocator(driver.Client, storageCredential, outputRelative, selected.Root.ID); e != nil {
			fail(e)
			return
		}
		if _, e = driver.Stat(ctx, selected.Root.ID); e != nil {
			fail(e)
			return
		}
		if e = a.store.UpdateStorageSourceRun(ctx, serverID, input.OperationKey, input.PlanDigest, nodeprotocol.StorageSourceEnumerating, "", selected.Root.ID, a.now()); e != nil {
			fail(e)
			return
		}
		run.OutputRootID = selected.Root.ID
		if e = a.store.ResetStorageSourceManifest(ctx, serverID, input.OperationKey, input.PlanDigest, a.now()); e != nil {
			fail(e)
			return
		}
		sort.Slice(selected.Entries, func(i, j int) bool { return selected.Entries[i].RelativePath < selected.Entries[j].RelativePath })
		files := make([]StorageSourceFile, 0, len(selected.Entries))
		hasher := sha256.New()
		var total int64
		for _, item := range selected.Entries {
			if item.IsDir {
				continue
			}
			relative := strings.TrimPrefix(item.RelativePath, "/")
			if item.Size <= 0 || relative == "" || normalizedExportPath(relative) != relative {
				fail(errors.New("node_storage_source_manifest_invalid"))
				return
			}
			sha := item.SHA1
			if !validSHA1(sha) {
				sha = ""
			}
			f := StorageSourceFile{OperationKey: input.OperationKey, Ordinal: int64(len(files)), ProviderFileID: item.ID, RelativePath: relative, Size: item.Size, SourceSHA1: sha, ChunkCount: int((item.Size + nodeprotocol.FileChunkSize - 1) / nodeprotocol.FileChunkSize)}
			files = append(files, f)
			total += item.Size
			manifestHashFile(hasher, f)
		}
		if e = a.store.SaveStorageSourceFileBatch(ctx, input.OperationKey, files, a.now()); e != nil {
			fail(e)
			return
		}
		if e = a.store.FinalizeStorageSourceManifest(ctx, serverID, input.OperationKey, input.PlanDigest, hex.EncodeToString(hasher.Sum(nil)), int64(len(files)), total, a.now()); e != nil {
			fail(e)
			return
		}
	}
	// Restore the frozen output path before reading a recovered manifest. An ID
	// alone must not trigger a whole-account search or accept a same-name folder.
	if e = configureQuarkDirectoryLocator(driver.Client, storageCredential, outputRelative, run.OutputRootID); e != nil {
		fail(e)
		return
	}
	proof := driver.Credentials().Proofs[0]
	if _, e = driver.ResolveScopedDirectory(ctx, credential.RootID, proof.Path, outputRelative, run.OutputRootID); e != nil {
		fail(e)
		return
	}
	for after := int64(-1); ; {
		files, err := a.store.StorageSourceFiles(ctx, input.OperationKey, after, storageSourcePageSize)
		if err != nil {
			fail(err)
			return
		}
		if len(files) == 0 {
			break
		}
		for _, f := range files {
			if e = guard(ctx); e != nil {
				fail(e)
				return
			}
			if f.Status != "completed" {
				if _, e = driver.ResolveScopedItem(ctx, run.OutputRootID, "/"+f.RelativePath, f.ProviderFileID); e != nil {
					fail(e)
					return
				}
			}
			if e = a.downloadQuarkSourceFile(ctx, serverID, input, f, driver, guard); e != nil {
				fail(e)
				return
			}
			after = f.Ordinal
			run, e = a.store.StorageSourceRun(ctx, serverID, input.OperationKey)
			if e != nil {
				fail(e)
				return
			}
			response = sourceResponse(input, run)
			response.CredentialExpiresAt = &grant.ExpiresAt
			response.Status = nodeprotocol.StorageSourceDownloading
			_ = a.store.UpdateActionReceipt(ctx, serverID, input.RequestID, response, a.now())
			_ = a.store.UpdateOperationExecution(ctx, serverID, input.OperationKey, input.PlanDigest, nodeprotocol.OperationRunning, nodeprotocol.PhasePullingSource, "", sourceProgress(run), a.now())
		}
	}
	if e = guard(ctx); e != nil {
		fail(e)
		return
	}
	export, e := a.publishStorageSourceExport(ctx, serverID, input, run)
	if e != nil {
		fail(e)
		return
	}
	if e = guard(ctx); e != nil {
		fail(e)
		return
	}
	run, e = a.store.StorageSourceRun(ctx, serverID, input.OperationKey)
	if e != nil {
		fail(e)
		return
	}
	response = sourceResponse(input, run)
	response.Status = nodeprotocol.StorageSourceCompleted
	response.FileExport = &export
	_ = a.store.CompleteStorageSourceAction(ctx, serverID, input.RequestID, input.OperationKey, input.PlanDigest, response, a.now())
}

// Without a provider content digest, each materialization generation reads one
// complete source and verifies it against a second complete, stable read. A
// restarted partial is overwritten, never mixed with a different generation.
func (a *Agent) downloadQuarkSourceFile(ctx context.Context, serverID string, input nodeprotocol.StorageSourceActionRequest, f StorageSourceFile, driver storageUploadDriver, guard func(context.Context) error) error {
	root, e := a.storageSourceRoot(input.OperationKey)
	if e != nil {
		return e
	}
	final, partial, e := secureStorageSourcePaths(root, f.RelativePath, f.Ordinal)
	if e != nil {
		return e
	}
	if f.Status == "completed" {
		sha, _, e := hashSourcePath(ctx, final, f.Size)
		if e != nil || sha != f.SHA256 {
			return errors.New(nodeprotocol.ErrorChecksumMismatch)
		}
		return guard(ctx)
	}
	before, e := driver.Stat(ctx, f.ProviderFileID)
	if e != nil {
		return e
	}
	if before.IsDir || before.Size != f.Size {
		return errors.New(nodeprotocol.ErrorReconciliationNeeded)
	}
	reader, ok := driver.(cloud.ReadDriver)
	if !ok {
		return errors.New(nodeprotocol.ErrorCapabilityMissing)
	}
	stream, e := reader.OpenRead(ctx, cloud.ReadRequest{FileID: f.ProviderFileID})
	if e != nil {
		return e
	}
	if stream.Body == nil {
		return errors.New(nodeprotocol.ErrorChecksumMismatch)
	}
	defer stream.Body.Close()
	if stream.TotalSize == nil || *stream.TotalSize != f.Size {
		return errors.New(nodeprotocol.ErrorChecksumMismatch)
	}
	handle, e := openStorageSourcePartial(partial, f.Size)
	if e != nil {
		return e
	}
	defer handle.Close()
	buffer := make([]byte, nodeprotocol.FileChunkSize)
	for index := 0; index < f.ChunkCount; index++ {
		if e = guard(ctx); e != nil {
			return e
		}
		offset := int64(index) * nodeprotocol.FileChunkSize
		size := min(nodeprotocol.FileChunkSize, f.Size-offset)
		payload := buffer[:size]
		if _, e = io.ReadFull(stream.Body, payload); e != nil {
			return e
		}
		if _, e = handle.WriteAt(payload, offset); e != nil {
			return e
		}
		if e = handle.Sync(); e != nil {
			return e
		}
		digest := sha256.Sum256(payload)
		if e = a.store.SaveStorageSourceChunk(ctx, input.OperationKey, f.Ordinal, nodeprotocol.FileChunkDigest{Index: index, Offset: offset, Size: size, SHA256: hex.EncodeToString(digest[:])}, a.now()); e != nil {
			return e
		}
	}
	extra := make([]byte, 1)
	if n, e := stream.Body.Read(extra); n != 0 || e != io.EOF {
		return errors.New(nodeprotocol.ErrorChecksumMismatch)
	}
	if e = handle.Close(); e != nil {
		return e
	}
	sha, sha1, e := hashSourcePath(ctx, partial, f.Size)
	if e != nil {
		return e
	}
	if f.SourceSHA1 != "" && !strings.EqualFold(sha1, f.SourceSHA1) {
		return errors.New(nodeprotocol.ErrorChecksumMismatch)
	}
	if _, e = verifyQuarkReadback(ctx, driver, before, before.ParentID, before.Name, f.Size, sha); e != nil {
		return e
	}
	if e = guard(ctx); e != nil {
		return e
	}
	if e = os.Link(partial, final); e != nil {
		if !errors.Is(e, os.ErrExist) {
			return e
		}
		published, _, err := hashSourcePath(ctx, final, f.Size)
		if err != nil || published != sha {
			return errors.New(nodeprotocol.ErrorChecksumMismatch)
		}
	}
	_ = os.Remove(partial)
	return a.store.CompleteStorageSourceFile(ctx, serverID, input.OperationKey, input.PlanDigest, f.Ordinal, sha, a.now())
}
