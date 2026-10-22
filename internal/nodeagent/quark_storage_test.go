package nodeagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newQuarkNodeTestAgent(t *testing.T) *Agent {
	t.Helper()
	root := t.TempDir()
	managed := filepath.Join(root, "managed")
	if e := os.Mkdir(managed, 0700); e != nil {
		t.Fatal(e)
	}
	store, e := OpenStore(filepath.Join(root, "node.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := Config{NodeID: "node-1", ListenAddress: "127.0.0.1:0", DataDirectory: root, ManagedRoot: managed, SealingPrivateKeyFile: filepath.Join(root, "seal.key"), MaxConcurrentOperations: 1, AllowInsecureDevelopment: true}
	if e = EnsureSealingIdentity(cfg); e != nil {
		t.Fatal(e)
	}
	a, e := New(cfg, store)
	if e != nil {
		t.Fatal(e)
	}
	return a
}
func TestQuarkNodeReceiptsEncryptedBoundAndCAS(t *testing.T) {
	a := newQuarkNodeTestAgent(t)
	ctx := context.Background()
	s, e := a.quarkReceipts("server-1", "operation-1")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Load(ctx, "missing"); !errors.Is(e, cloud.ErrOperationNotFound) {
		t.Fatal("a missing native receipt must use the absence sentinel", e)
	}
	r := cloud.OperationReceipt{ID: "receipt-1", Provider: cloud.ProviderQuark, Route: "agent", AccountID: "private-account", Kind: "upload", Revision: 1, State: "prepared", Payload: json.RawMessage(`{"upload_url":"https://upload.example/?secret=private-upload-token"}`), UpdatedAt: time.Now()}
	if e = s.Create(ctx, r); e != nil {
		t.Fatal(e)
	}
	if e = s.Create(ctx, r); e == nil {
		t.Fatal("duplicate accepted")
	}
	var raw []byte
	if e = a.store.db.QueryRow(`SELECT ciphertext FROM cloud_operation_receipts`).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	for _, secret := range []string{"private-account", "upload.example", "private-upload-token"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("private receipt plaintext")
		}
	}
	got, e := s.Load(ctx, r.ID)
	if e != nil || got.AccountID != r.AccountID || !bytes.Equal(got.Payload, r.Payload) {
		t.Fatal("receipt roundtrip failed", e)
	}
	wrong, _ := a.quarkReceipts("other-server", "operation-1")
	if _, e = wrong.Load(ctx, r.ID); e == nil {
		t.Fatal("cross-server read")
	}
	wrong, _ = a.quarkReceipts("server-1", "other-op")
	if _, e = wrong.Load(ctx, r.ID); e == nil {
		t.Fatal("cross-operation read")
	}
	changed := r
	changed.Revision++
	changed.AccountID = "other-account"
	if e = s.Update(ctx, 1, changed); e == nil {
		t.Fatal("account drift accepted")
	}
	r.Revision = 2
	r.State = "accepted"
	if e = s.Update(ctx, 1, r); e != nil {
		t.Fatal(e)
	}
	if e = s.Update(ctx, 1, r); e == nil {
		t.Fatal("stale receipt accepted")
	}
	if _, e = a.store.db.Exec(`UPDATE cloud_operation_receipts SET revision=3`); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Load(ctx, r.ID); e == nil || errors.Is(e, cloud.ErrOperationNotFound) {
		t.Fatal("AAD revision tamper accepted")
	}
}

func TestQuarkNodeReceiptStorageFailureIsNotAbsence(t *testing.T) {
	a := newQuarkNodeTestAgent(t)
	s, err := a.quarkReceipts("server-1", "operation-1")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Load(context.Background(), "missing"); err == nil || errors.Is(err, cloud.ErrOperationNotFound) {
		t.Fatal("closed receipt storage was treated as an absent operation")
	}
}

type quarkReadbackFake struct {
	*fakeStorageDriver
	body           []byte
	corrupt        bool
	changeRevision bool
	reads          int
	failReads      int
}

func newQuarkReadbackFake(body []byte) *quarkReadbackFake {
	return &quarkReadbackFake{fakeStorageDriver: newFakeStorageDriver(nil), body: body}
}
func (d *quarkReadbackFake) Provider() string { return cloud.ProviderQuark }
func (d *quarkReadbackFake) OpenRead(_ context.Context, r cloud.ReadRequest) (cloud.ReadResult, error) {
	d.reads++
	if d.failReads > 0 {
		d.failReads--
		return cloud.ReadResult{}, errors.New("temporary read failure")
	}
	body := append([]byte(nil), d.body...)
	if d.corrupt && len(body) > 0 {
		body[0] ^= 1
	}
	size := int64(len(body))
	if d.changeRevision {
		x := d.items[r.FileID]
		x.Revision = "changed"
		d.items[r.FileID] = x
	}
	return cloud.ReadResult{Body: io.NopCloser(bytes.NewReader(body)), TotalSize: &size}, nil
}
func TestQuarkNodeReadbackRejectsCorruptionRevisionAndDuplicateLocation(t *testing.T) {
	body := []byte("exact original content")
	sum := sha256.Sum256(body)
	expected := hex.EncodeToString(sum[:])
	item := cloud.Item{ID: "file-1", ParentID: "0", Name: "video.mkv", Size: int64(len(body)), Revision: "initial"}
	for _, mode := range []string{"success", "corrupt", "revision", "duplicate", "size"} {
		t.Run(mode, func(t *testing.T) {
			d := newQuarkReadbackFake(body)
			d.items[item.ID] = item
			d.children["0"] = []string{item.ID}
			switch mode {
			case "corrupt":
				d.corrupt = true
			case "revision":
				d.changeRevision = true
			case "duplicate":
				other := item
				other.ID = "other"
				d.items["other"] = other
				d.children["0"] = append(d.children["0"], "other")
			case "size":
				d.body = d.body[:len(body)-1]
			}
			_, e := verifyQuarkReadback(context.Background(), d, item, "0", item.Name, item.Size, expected)
			if (e == nil) != (mode == "success") {
				t.Fatalf("mode %s error %v", mode, e)
			}
		})
	}
}
func TestQuarkNodeUploadRestartRechecksOutputWithoutUploadingTwice(t *testing.T) {
	a := newQuarkNodeTestAgent(t)
	ctx := context.Background()
	body := []byte("original video")
	if e := os.WriteFile(filepath.Join(a.config.ManagedRoot, "video.mkv"), body, 0600); e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC()
	export, e := buildFileExport(ctx, a.config.ManagedRoot, "server-1", "export-1", ManagedDownload{TaskID: "task-1", NodeLocalRoot: a.config.ManagedRoot}, downloadpkg.Manifest{Complete: true, Files: []downloadpkg.File{{RelativePath: "video.mkv", Size: int64(len(body))}}}, now, now.Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	if e = a.store.SaveFileExport(ctx, "server-1", export); e != nil {
		t.Fatal(e)
	}
	f := export.Files[0].Public
	plan := nodeprotocol.StorageUploadPlan{DigestVersion: 2, StorageID: "storage-1", ProviderType: cloud.ProviderQuark, SourceExportOperationKey: "export-1", SourceManifestDigest: export.Summary.ManifestDigest, TargetRootID: "0", Files: []nodeprotocol.StorageUploadFile{{SourceFileToken: f.FileToken, TargetRelativePath: "video.mkv", Size: f.Size, SHA256: f.SHA256, ConflictAction: nodeprotocol.StorageConflictFailIfExists}}}
	raw, _ := json.Marshal(plan)
	op := nodeprotocol.OperationPlan{ProtocolVersion: 1, OperationKey: "upload-1", TaskID: "task-1", NodeID: "node-1", Kind: nodeprotocol.OperationKindStorageUpload, PlanRevision: 1, LeaseEpoch: 1, LeaseExpiresAt: now.Add(time.Hour), Payload: raw}
	digest, _ := op.Digest()
	if _, _, e = a.store.Put(ctx, "server-1", digest, op, now); e != nil {
		t.Fatal(e)
	}
	input := nodeprotocol.StorageActionRequest{OperationKey: op.OperationKey, TaskID: op.TaskID, PlanDigest: digest}
	d := newQuarkReadbackFake(body)
	d.failReads = 1
	guard := func(context.Context) error { return nil }
	if _, e = a.uploadQuarkStorageFile(ctx, "server-1", input, plan, plan.Files[0], d, guard); e == nil {
		t.Fatal("readback failure completed")
	}
	checkpoint, e := a.store.StorageUploadCheckpoint(ctx, op.OperationKey, f.FileToken)
	if e != nil || checkpoint.TargetItemID == "" || checkpoint.Status == nodeprotocol.StorageFileCompleted {
		t.Fatal("exact output not checkpointed before failed readback", e)
	}
	restarted, e := New(a.config, a.store)
	if e != nil {
		t.Fatal(e)
	}
	result, e := restarted.uploadQuarkStorageFile(ctx, "server-1", input, plan, plan.Files[0], d, guard)
	if e != nil || result.Status != nodeprotocol.StorageFileCompleted || result.ContentDigest == nil || !result.ContentDigest.ValidReadbackSHA256(f.SHA256) || d.uploadCalls != 1 || result.SHA1 != "" {
		t.Fatalf("result=%+v uploads=%d error=%v", result, d.uploadCalls, e)
	}
}
func TestQuarkNodeGrantRejectsUnscopedAndLongLivedCredentials(t *testing.T) {
	a := &quark.Authorization{AccessToken: "a", DeviceID: "d", AccountID: "acct", Generation: 1, IdentityVerified: true}
	p := quark.SpaceProof{Version: 1, Generation: 1, TVAccount: "acct", AgentAccount: "acct", TVRoot: "root", AgentRoot: "agent-root", ContentSHA256: strings.Repeat("a", 64), VerifiedAt: time.Now()}
	for _, mode := range []string{"valid", "refresh", "web", "root", "extra-root"} {
		t.Run(mode, func(t *testing.T) {
			tv, agent := *a, *a
			b := quark.Credentials{Version: 1, TV: &tv, Agent: &agent, Proofs: []quark.SpaceProof{p}}
			switch mode {
			case "refresh":
				b.TV.RefreshToken = "secret"
			case "web":
				b.Web = &quark.WebAuthorization{Cookie: "secret"}
			case "root":
				b.Proofs[0].TVRoot = "other"
			case "extra-root":
				b.Proofs = append(b.Proofs, p)
			}
			raw, _ := json.Marshal(b)
			_, e := validateQuarkGrant(nodeprotocol.StorageCredential{ProviderType: cloud.ProviderQuark, Quark: raw, RootID: "root"})
			if (e == nil) != (mode == "valid") {
				t.Fatal(mode, e)
			}
		})
	}
}

func quarkSourceFileFixture(t *testing.T) (*Agent, nodeprotocol.StorageSourceActionRequest, StorageSourceFile, *quarkReadbackFake) {
	t.Helper()
	a := newQuarkNodeTestAgent(t)
	ctx := context.Background()
	body := []byte("selected share original")
	now := time.Now().UTC()
	plan := nodeprotocol.StorageSourcePlan{DigestVersion: 2, TaskDirectoryName: "omc-task", StorageID: "storage-1", ProviderType: cloud.ProviderQuark, SourceKind: nodeprotocol.StorageSourceKindQuarkShareSelected, TargetKind: nodeprotocol.StorageSourceTargetLocal, SourceIdentityDigest: strings.Repeat("a", 64), SourceContentDigest: strings.Repeat("b", 64), OfflineDestinationID: "root"}
	raw, _ := json.Marshal(plan)
	op := nodeprotocol.OperationPlan{ProtocolVersion: 1, OperationKey: "source-quark", TaskID: "task-1", NodeID: "node-1", Kind: nodeprotocol.OperationKindStorageSourceMaterialize, PlanRevision: 1, LeaseEpoch: 1, LeaseExpiresAt: now.Add(time.Hour), Payload: raw}
	digest, _ := op.Digest()
	if _, _, e := a.store.Put(ctx, "server-1", digest, op, now); e != nil {
		t.Fatal(e)
	}
	input := nodeprotocol.StorageSourceActionRequest{OperationKey: op.OperationKey, TaskID: op.TaskID, PlanDigest: digest, StorageID: plan.StorageID}
	if _, e := a.store.PrepareStorageSourceRun(ctx, "server-1", input, now); e != nil {
		t.Fatal(e)
	}
	file := StorageSourceFile{OperationKey: op.OperationKey, Ordinal: 0, ProviderFileID: "original-1", RelativePath: "Movie.mkv", Size: int64(len(body)), ChunkCount: 1}
	if e := a.store.SaveStorageSourceFileBatch(ctx, op.OperationKey, []StorageSourceFile{file}, now); e != nil {
		t.Fatal(e)
	}
	if e := a.store.FinalizeStorageSourceManifest(ctx, "server-1", op.OperationKey, digest, strings.Repeat("c", 64), 1, file.Size, now); e != nil {
		t.Fatal(e)
	}
	fake := newQuarkReadbackFake(body)
	fake.items[file.ProviderFileID] = cloud.Item{ID: file.ProviderFileID, Name: "Movie.mkv", ParentID: "0", Size: file.Size}
	fake.children["0"] = []string{file.ProviderFileID}
	return a, input, file, fake
}
func TestQuarkNodeSourceMaterializesAndReusesOnlyVerifiedCompleteFiles(t *testing.T) {
	a, input, file, fake := quarkSourceFileFixture(t)
	ctx := context.Background()
	guard := func(context.Context) error { return nil }
	if e := a.downloadQuarkSourceFile(ctx, "server-1", input, file, fake, guard); e != nil {
		t.Fatal(e)
	}
	if fake.reads != 2 {
		t.Fatalf("expected download and full verification, got %d reads", fake.reads)
	}
	files, e := a.store.StorageSourceFiles(ctx, input.OperationKey, -1, 10)
	if e != nil || len(files) != 1 || files[0].Status != "completed" || !nodeprotocol.ValidDigest(files[0].SHA256) {
		t.Fatal("unverified source marked complete", e)
	}
	// The confirmed local export does not depend on the share publisher or cloud
	// being reachable after completion.
	fake.failReads = 10
	if e = a.downloadQuarkSourceFile(ctx, "server-1", input, files[0], fake, guard); e != nil || fake.reads != 2 {
		t.Fatal("completed source was redownloaded", e)
	}
	run, e := a.store.StorageSourceRun(ctx, "server-1", input.OperationKey)
	if e != nil {
		t.Fatal(e)
	}
	export, e := a.publishStorageSourceExport(ctx, "server-1", input, run)
	if e != nil || export.TotalFiles != 1 {
		t.Fatal("verified file not exported", e)
	}
}
func TestQuarkNodeSourceInterruptedReadCannotPublishIncompleteFile(t *testing.T) {
	a, input, file, fake := quarkSourceFileFixture(t)
	calls := 0
	guard := func(context.Context) error {
		calls++
		if calls == 2 {
			return errors.New(nodeprotocol.ErrorCredentialExpired)
		}
		return nil
	}
	if e := a.downloadQuarkSourceFile(context.Background(), "server-1", input, file, fake, guard); e == nil {
		t.Fatal("expired grant completed")
	}
	run, e := a.store.StorageSourceRun(context.Background(), "server-1", input.OperationKey)
	if e != nil || run.DownloadedFiles != 0 {
		t.Fatal("unpublished read became complete", e)
	}
	if _, e = a.publishStorageSourceExport(context.Background(), "server-1", input, run); e == nil {
		t.Fatal("incomplete source exported")
	}
	if e = a.downloadQuarkSourceFile(context.Background(), "server-1", input, file, fake, func(context.Context) error { return nil }); e != nil {
		t.Fatal(e)
	}
}

func TestQuarkNodeGrantContinuationRequiresSameIdentityAndRoot(t *testing.T) {
	for _, mode := range []string{"renew", "account-drift", "root-drift", "expired"} {
		t.Run(mode, func(t *testing.T) {
			a, input, _, _ := quarkSourceFileFixture(t)
			ctx := context.Background()
			now := time.Now().UTC()
			a.now = func() time.Time { return now }
			tv := quark.Authorization{AccessToken: "tv", DeviceID: "tv-device", AccountID: "tv-account", Generation: 1, IdentityVerified: true}
			agent := quark.Authorization{AccessToken: "agent", DeviceID: "agent-device", AccountID: "agent-account", Generation: 1, IdentityVerified: true}
			proof := quark.SpaceProof{Version: 1, Generation: 1, TVAccount: tv.AccountID, AgentAccount: agent.AccountID, TVRoot: "root", AgentRoot: "agent-root", ContentSHA256: strings.Repeat("a", 64), VerifiedAt: now}
			bundle := quark.Credentials{Version: 1, Revision: 2, TV: &tv, Agent: &agent, Proofs: []quark.SpaceProof{proof}}
			raw, _ := json.Marshal(bundle)
			credential := nodeprotocol.StorageCredential{ProviderType: cloud.ProviderQuark, RootID: "root", Quark: raw}
			grant := nodeprotocol.CredentialGrant{GrantID: "grant-old", NodeID: "node-1", TaskID: input.TaskID, OperationKey: input.OperationKey, ResourceKind: nodeprotocol.ResourceKindStorage, ResourceID: input.StorageID, CredentialRevision: 2, AllowedActions: []string{nodeprotocol.StorageSourceGrantRead}, ExpiresAt: now.Add(time.Minute)}
			guard := a.quarkGrantGuard("server-1", input.PlanDigest, grant, credential)
			next := grant
			next.GrantID = "grant-new"
			next.CredentialRevision++
			next.ExpiresAt = now.Add(10 * time.Minute)
			bundle.Revision++
			switch mode {
			case "account-drift":
				bundle.TV.AccountID = "other-account"
				bundle.Proofs[0].TVAccount = "other-account"
			case "root-drift":
				bundle.Proofs[0].TVRoot = "other-root"
				credential.RootID = "other-root"
			}
			raw, _ = json.Marshal(bundle)
			credential.Quark = raw
			next.Credential, _ = json.Marshal(credential)
			if mode != "expired" {
				sealed, e := nodeprotocol.SealCredentialGrant(a.sealingPublicKey, next, now)
				if e != nil {
					t.Fatal(e)
				}
				if e = a.store.SaveCredentialGrant(ctx, "server-1", sealed, now); e != nil {
					t.Fatal(e)
				}
			}
			now = now.Add(90 * time.Second)
			e := guard(ctx)
			if (e == nil) != (mode == "renew") {
				t.Fatalf("mode %s error %v", mode, e)
			}
		})
	}
}
