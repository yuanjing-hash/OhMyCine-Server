//go:build quark_live

package handlers_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/nodeagent"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/nodeclient"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

type nodeProbeChildInput struct {
	Root    string
	Spec    nodeProbeSpec
	Options nodeagent.QuarkValidationNodeOptions
	Offline bool
}
type nodeProbeChildSummary struct {
	Done                                                      bool
	Requests, API, Mkdir, Pre, Put, Finish, Reads, ReadPasses int
	Upload, Read                                              int64
	Stopped                                                   bool
	PartCount, ConfirmedParts                                 int
	Fixture                                                   *nodeProbeFixtureSummary `json:",omitempty"`
}

func TestQuarkLiveNodeProbeChild(t *testing.T) {
	if os.Getenv("OMC_QUARK_NODE_PROBE_CHILD") != "1" {
		t.Skip("dedicated child entry")
	}
	input := bufio.NewReaderSize(os.Stdin, 64<<10)
	line, e := input.ReadBytes('\n')
	if e != nil || len(line) > 64<<10 {
		t.Fatal("child_input_rejected")
	}
	var in nodeProbeChildInput
	if liveTVDecode(line, &in) != nil || !in.Spec.valid() || observationCheckPrivateDirectory(in.Root) != nil || in.Options.DataDirectory != filepath.Join(in.Root, "node-state") || in.Options.ManagedRoot != filepath.Join(in.Root, "source") || in.Options.SourceSHA256 != in.Spec.SourceSHA || in.Options.SourceSize != nodeProbeSize || in.Options.SourceName != "sample.bin" || in.Options.NodeID != in.Spec.nodeID() || in.Options.ServerID != in.Spec.serverID() || in.Options.TaskID != in.Spec.taskID() || in.Options.ExportOperationKey != in.Spec.exportID() {
		t.Fatal("child_scope_rejected")
	}
	if !in.Offline && filepath.Clean(in.Root) != filepath.Clean(filepath.FromSlash(nodeProbeDirectory)) {
		t.Fatal("child_fixed_live_batch_required")
	}
	ctx, cancel := context.WithDeadline(context.Background(), in.Spec.Started.Add(nodeProbeLifetime))
	defer cancel()
	go func() { _, _ = io.Copy(io.Discard, input); cancel() }()
	g, e := openNodeProbeGuard(in.Root, in.Spec)
	if e != nil {
		t.Fatal("child_guard_rejected")
	}
	defer g.journal.lock.Close()
	var fixture *nodeProbeFixture
	if in.Offline {
		fixture, e = newNodeProbeFixture(in.Spec, filepath.Join(in.Root, "source", "sample.bin"))
		if e != nil {
			t.Fatal("child_fixture_rejected")
		}
		g.delay = 0
	}
	wrap := func(c cloud.Config, base http.RoundTripper, media bool) (http.RoundTripper, error) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if e := g.bind(c); e != nil {
			return nil, e
		}
		if fixture != nil {
			base = fixture
		}
		return g.wrap(base, media), nil
	}
	e = nodeagent.RunQuarkValidationNode(ctx, in.Options, wrap, func(ready nodeagent.QuarkValidationNodeReady) error { return json.NewEncoder(os.Stdout).Encode(ready) })
	if e != nil {
		t.Fatal("child_runtime_failed")
	}
	s := g.state
	summary := nodeProbeChildSummary{Done: true, Requests: s.Requests, API: s.API, Mkdir: s.Mkdir, Pre: s.Pre, Put: s.Put, Finish: s.Finish, Reads: s.Reads, ReadPasses: s.ReadPasses, Upload: s.UploadActual, Read: s.ReadActual, Stopped: s.Stopped != ""}
	summary.PartCount, summary.ConfirmedParts = s.PartCount, len(s.ETags)
	if fixture != nil {
		stats := fixture.Snapshot()
		summary.Fixture = &stats
	}
	if json.NewEncoder(os.Stdout).Encode(summary) != nil {
		t.Fatal("child_summary_failed")
	}
}

type nodeProbeProcess struct {
	stdin io.WriteCloser
	cmd   *exec.Cmd
	lines chan []byte
	ready nodeagent.QuarkValidationNodeReady
}

func startNodeProbe(t *testing.T, root string, c *nodeProbeController, offline bool) *nodeProbeProcess {
	t.Helper()
	binary, e := os.Executable()
	if e != nil {
		t.Fatal("child_binary_unavailable")
	}
	cmd := exec.Command(binary, "-test.run=^TestQuarkLiveNodeProbeChild$", "-test.timeout=16m")
	for _, env := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(env), "OMC_QUARK_") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "OMC_QUARK_NODE_PROBE_CHILD=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	stdin, e := cmd.StdinPipe()
	if e != nil {
		t.Fatal("child_pipe_failed")
	}
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		t.Fatal("child_pipe_failed")
	}
	cmd.Stderr = io.Discard
	if e = cmd.Start(); e != nil {
		t.Fatal("child_start_failed")
	}
	p := &nodeProbeProcess{stdin: stdin, cmd: cmd, lines: make(chan []byte, 16)}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			stdin.Close()
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	go func() {
		defer close(p.lines)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		for scanner.Scan() {
			p.lines <- append([]byte(nil), scanner.Bytes()...)
		}
	}()
	s := c.Spec
	opts := nodeagent.QuarkValidationNodeOptions{NodeID: s.nodeID(), ServerID: s.serverID(), TaskID: s.taskID(), ExportOperationKey: s.exportID(), DataDirectory: filepath.Join(root, "node-state"), ManagedRoot: filepath.Join(root, "source"), SourceName: "sample.bin", SourceSize: nodeProbeSize, SourceSHA256: s.SourceSHA, EnrollmentToken: c.EnrollmentToken}
	if e = json.NewEncoder(stdin).Encode(nodeProbeChildInput{Root: root, Spec: s, Options: opts, Offline: offline}); e != nil {
		t.Fatal("child_input_failed")
	}
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line, ok := <-p.lines:
			if !ok {
				t.Fatal("child_exited_before_ready")
			}
			if json.Unmarshal(line, &p.ready) == nil && p.ready.Socket != "" {
				if p.ready.PID != cmd.Process.Pid || p.ready.PID == os.Getpid() {
					t.Fatal("node_is_not_separate_process")
				}
				return p
			}
		case <-timer.C:
			t.Fatal("child_ready_timeout")
		}
	}
}
func stopNodeProbe(t *testing.T, p *nodeProbeProcess) nodeProbeChildSummary {
	t.Helper()
	p.stdin.Close()
	var summary nodeProbeChildSummary
	timer := time.NewTimer(40 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line, ok := <-p.lines:
			if !ok {
				if e := p.cmd.Wait(); e != nil || !summary.Done {
					t.Fatal("node_clean_shutdown_failed")
				}
				return summary
			}
			var s nodeProbeChildSummary
			if json.Unmarshal(line, &s) == nil && s.Done {
				summary = s
			}
		case <-timer.C:
			p.cmd.Process.Kill()
			p.cmd.Wait()
			t.Fatal("node_shutdown_timeout")
		}
	}
}

func executeNodeProbe(t *testing.T, root string, store *nodeProbeJSON, c *nodeProbeController, b quark.Credentials, offline bool) {
	t.Helper()
	s := c.Spec
	ctx, cancel := context.WithDeadline(context.Background(), s.Started.Add(nodeProbeLifetime))
	defer cancel()
	identity, e := nodeclient.ParseIdentity(s.serverID(), c.CertificatePEM, c.PrivateKeyPEM)
	if e != nil {
		t.Fatal("controller_identity_rejected")
	}
	p := startNodeProbe(t, root, c, offline)
	if c.Enrolled.CertificateFingerprint == "" {
		c.Enrolled, e = nodeclient.EnrollQuarkValidationNode(ctx, p.ready.Socket, s.nodeID(), c.EnrollmentToken, identity)
		if e != nil || store.save(c) != nil {
			t.Fatal("node_enrollment_failed")
		}
	}
	client, e := nodeclient.NewQuarkValidationNodeClient(p.ready.Socket, c.Enrolled.CertificateFingerprint, identity)
	if e != nil {
		t.Fatal("node_client_failed")
	}
	health, e := client.Health(ctx)
	if e != nil || health.NodeID != s.nodeID() {
		t.Fatal("signed_node_health_failed")
	}
	manifest, e := client.FileExportManifest(ctx, s.exportID(), s.taskID(), 1, 1)
	if e != nil || len(manifest.Files) != 1 || manifest.Files[0].SHA256 != s.SourceSHA || manifest.Files[0].Size != nodeProbeSize {
		t.Fatal("node_export_source_mismatch")
	}
	if !c.Prepared {
		file := manifest.Files[0]
		upload := nodeprotocol.StorageUploadPlan{DigestVersion: 2, StorageID: s.storageID(), ProviderType: nodeprotocol.StorageProviderQuark, SourceExportOperationKey: s.exportID(), SourceManifestDigest: manifest.Summary.ManifestDigest, TargetRootID: s.Identity.Proof.TVRoot, Files: []nodeprotocol.StorageUploadFile{{SourceFileToken: file.FileToken, TargetRelativePath: s.DirectoryName + "/" + s.UploadName, Size: file.Size, SHA256: file.SHA256, ConflictAction: nodeprotocol.StorageConflictFailIfExists}}}
		payload, _ := json.Marshal(upload)
		c.Plan = nodeprotocol.OperationPlan{ProtocolVersion: 1, OperationKey: s.operationID(), TaskID: s.taskID(), NodeID: s.nodeID(), Kind: nodeprotocol.OperationKindStorageUpload, PlanRevision: 1, LeaseEpoch: 1, LeaseExpiresAt: s.Started.Add(nodeProbeLifetime), Payload: payload}
		c.PlanDigest, e = c.Plan.Digest()
		if e != nil {
			t.Fatal("node_plan_invalid")
		}
		bundle, _ := json.Marshal(b)
		credential, _ := json.Marshal(nodeprotocol.StorageCredential{ProviderType: nodeprotocol.StorageProviderQuark, RootID: upload.TargetRootID, Quark: bundle})
		grant := nodeprotocol.CredentialGrant{GrantID: "quark-validation-grant-" + s.Nonce, NodeID: s.nodeID(), TaskID: s.taskID(), OperationKey: s.operationID(), ResourceKind: nodeprotocol.ResourceKindStorage, ResourceID: s.storageID(), CredentialRevision: b.Revision, AllowedActions: []string{nodeprotocol.StorageActionUpload}, ExpiresAt: s.Started.Add(nodeProbeLifetime), Credential: credential}
		c.Envelope, e = nodeprotocol.SealCredentialGrant(c.Enrolled.EncryptionPublicKey, grant, time.Now())
		if e != nil {
			t.Fatal("node_sealed_grant_failed")
		}
		c.Request = nodeprotocol.StorageActionRequest{RequestID: s.requestID(), TaskID: s.taskID(), OperationKey: s.operationID(), PlanDigest: c.PlanDigest, StorageID: s.storageID(), Action: nodeprotocol.StorageActionUpload, GrantID: grant.GrantID}
		c.Prepared = true
		if store.save(c) != nil {
			t.Fatal("node_frozen_plan_save_failed")
		}
	}
	if !c.Completed {
		if _, e = client.PutOperation(ctx, nodeprotocol.PutOperationRequest{Plan: c.Plan, PlanDigest: c.PlanDigest}); e != nil {
			t.Fatal("node_plan_submit_failed")
		}
		if e = client.PutCredentialGrant(ctx, c.Envelope); e != nil {
			t.Fatal("node_grant_submit_failed")
		}
		if _, e = client.StorageAction(ctx, c.Request); e != nil {
			t.Fatal("node_action_submit_failed")
		}
		lastProgress := time.Now()
		for {
			result, e := client.StorageActionResult(ctx, c.Request.RequestID, s.taskID(), s.operationID())
			if e != nil {
				t.Fatal("node_result_query_failed")
			}
			if result.ErrorCode != "" {
				t.Logf("node_result status=%s error_code=%s completed_files=%d", result.Status, result.ErrorCode, result.CompletedFiles)
				stopNodeProbe(t, p)
				t.Fatal("node_cloud_operation_incomplete")
			}
			if result.Status == nodeprotocol.StorageActionCompleted {
				c.Result = result
				if !nodeProbeResultMatches(c) {
					t.Fatal("node_result_binding_mismatch")
				}
				c.Completed = true
				if store.save(c) != nil {
					t.Fatal("node_result_save_failed")
				}
				break
			}
			if time.Since(lastProgress) > 20*time.Second {
				t.Logf("node_progress status=%s completed_files=%d total_files=%d", result.Status, result.CompletedFiles, len(manifest.Files))
				lastProgress = time.Now()
			}
			select {
			case <-ctx.Done():
				t.Fatal("node_batch_deadline_exceeded")
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
	first := stopNodeProbe(t, p)
	if first.Stopped || first.Mkdir != 1 || first.Pre != 1 || first.PartCount < 2 || first.PartCount > 8 || first.Put != first.PartCount || first.ConfirmedParts != first.PartCount || offline && first.PartCount != 3 || first.Finish != 1 || first.ReadPasses != 3 || first.Upload != nodeProbeSize || first.Read != 3*nodeProbeSize || !nodeProbeResultMatches(c) {
		t.Logf("node_counts requests=%d api=%d mkdir=%d pre=%d put=%d finish=%d reads=%d passes=%d uploaded=%d read=%d stopped=%t", first.Requests, first.API, first.Mkdir, first.Pre, first.Put, first.Finish, first.Reads, first.ReadPasses, first.Upload, first.Read, first.Stopped)
		t.Fatal("node_delivery_evidence_incomplete")
	}
	// A second OS process opens the original Node store and answers the original
	// result request. No fresh cloud client, write or counter reset is involved.
	p = startNodeProbe(t, root, c, offline)
	client, e = nodeclient.NewQuarkValidationNodeClient(p.ready.Socket, c.Enrolled.CertificateFingerprint, identity)
	if e != nil {
		t.Fatal("node_restart_client_failed")
	}
	loaded, e := client.StorageActionResult(ctx, c.Request.RequestID, s.taskID(), s.operationID())
	if e != nil || !reflect.DeepEqual(loaded, c.Result) {
		t.Fatal("node_restart_lost_durable_result")
	}
	second := stopNodeProbe(t, p)
	first.Fixture = nil
	second.Fixture = nil
	if !reflect.DeepEqual(first, second) {
		t.Fatal("node_restart_replayed_cloud_or_reset_budget")
	}
	if !nodeProbeSourceUnchanged(root, s) {
		t.Fatal("node_source_changed")
	}
	t.Logf("node_probe_complete separate_os_process=true sealed_grant=true source_unchanged=true restarted_result_cloud_requests=0 api=%d requests=%d uploaded=%d read=%d catalog_publish_verified=false", first.API, first.Requests, first.Upload, first.Read)
}

func nodeProbeResultMatches(c *nodeProbeController) bool {
	r, s := c.Result, c.Spec
	var plan nodeprotocol.StorageUploadPlan
	digest, err := c.Plan.Digest()
	if err != nil || digest != c.PlanDigest || c.Request.PlanDigest != digest || r.PlanDigest != digest || r.RequestID != s.requestID() || r.OperationKey != s.operationID() || r.Status != nodeprotocol.StorageActionCompleted || r.ErrorCode != "" || r.TotalFiles != 1 || r.CompletedFiles != 1 || len(r.Files) != 1 || json.Unmarshal(c.Plan.Payload, &plan) != nil || plan.Validate() != nil || len(plan.Files) != 1 {
		return false
	}
	f := r.Files[0]
	return f.SourceFileToken == plan.Files[0].SourceFileToken && f.TargetName == s.UploadName && f.Size == nodeProbeSize && f.Status == nodeprotocol.StorageFileCompleted && f.TargetParentID != "" && f.TargetItemID != "" && f.ContentDigest != nil && f.ContentDigest.ValidReadbackSHA256(s.SourceSHA)
}

func TestQuarkNodeProbeOfflineProcessSuccess(t *testing.T) {
	root := observationOfflinePrivateDir(t)
	var e error
	root, e = filepath.EvalSymlinks(root)
	if e != nil {
		t.Fatal("canonical fixture root failed")
	}
	p := quark.SpaceProof{Version: 1, Generation: 1, TVAccount: "node-tv-account", AgentAccount: "node-agent-account", TVRoot: quark.QualifyID(quark.RouteTV, "node-tv-account", "T"), AgentRoot: quark.QualifyID(quark.RouteAgent, "node-agent-account", "A"), Path: "/owned", ContentSHA256: strings.Repeat("a", 64), VerifiedAt: time.Now().UTC()}
	b := quark.Credentials{Version: 1, Revision: 1, TV: &quark.Authorization{AccessToken: "synthetic-tv-access", DeviceID: "synthetic-tv-device", AccountID: p.TVAccount, Generation: 1, IdentityVerified: true}, Agent: &quark.Authorization{AccessToken: "synthetic-agent-access", DeviceID: "synthetic-agent-device", ClientDeviceID: "synthetic-agent-client", AccountID: p.AgentAccount, Generation: 1, IdentityVerified: true}, Proofs: []quark.SpaceProof{p}}
	i := followIdentity{TVAccount: p.TVAccount, AgentAccount: p.AgentAccount, Proof: p, ProofRootACK: "A"}
	store, c, e := nodeProbePrepareController(root, i)
	if e != nil {
		t.Fatal("synthetic controller failed")
	}
	defer store.lock.Close()
	executeNodeProbe(t, root, store, c, b, true)
	for name, change := range map[string]func(*nodeprotocol.StorageActionResponse){
		"request":    func(r *nodeprotocol.StorageActionResponse) { r.RequestID += "other" },
		"operation":  func(r *nodeprotocol.StorageActionResponse) { r.OperationKey += "other" },
		"plan":       func(r *nodeprotocol.StorageActionResponse) { r.PlanDigest = strings.Repeat("0", 64) },
		"file_token": func(r *nodeprotocol.StorageActionResponse) { r.Files[0].SourceFileToken += "other" },
		"name":       func(r *nodeprotocol.StorageActionResponse) { r.Files[0].TargetName += "other" },
		"size":       func(r *nodeprotocol.StorageActionResponse) { r.Files[0].Size-- },
	} {
		candidate := *c
		candidate.Result.Files = append([]nodeprotocol.StorageUploadFileResult(nil), c.Result.Files...)
		change(&candidate.Result)
		if nodeProbeResultMatches(&candidate) {
			t.Fatalf("wrong %s result accepted", name)
		}
	}
}
