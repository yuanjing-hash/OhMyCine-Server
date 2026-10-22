//go:build quark_live

package handlers_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQuarkLiveLargeFile(t *testing.T)          { runQuarkLargeFile(t, false) }
func TestQuarkLiveLargeFilePreflight(t *testing.T) { runQuarkLargeFile(t, true) }
func TestQuarkLiveLargeFileInspect(t *testing.T) {
	dir := os.Getenv("OMC_QUARK_LARGEFILE_INSPECT")
	if dir == "" {
		t.Skip("explicit fixed batch required")
	}
	if largefileValidateLiveProfile(largefileStandardProfile, dir) != nil || observationCheckPrivateDirectory(dir) != nil {
		t.Fatal("largefile_inspection_rejected")
	}
	lock, e := database.AcquireExclusiveRuntime(filepath.Join(dir, "largefile"))
	if e != nil {
		t.Fatal("largefile_batch_busy")
	}
	defer lock.Close()
	raw, e := liveTVRead(filepath.Join(dir, "largefile.enc"), 48<<20, false)
	if e != nil {
		t.Fatal("largefile_journal_unavailable")
	}
	key, e := liveTVRead(filepath.Join(dir, "largefile.key"), 1024, false)
	if e != nil {
		t.Fatal("largefile_key_unavailable")
	}
	cipher, e := credential.Open("", string(key))
	if e != nil {
		t.Fatal("largefile_key_invalid")
	}
	plain, e := cipher.Decrypt(largefilePurpose, string(raw))
	if e != nil {
		t.Fatal("largefile_journal_invalid")
	}
	var st largefileState
	if liveTVDecode([]byte(plain), &st) != nil || !largefileManifest(st) {
		t.Fatal("largefile_manifest_invalid")
	}
	t.Logf("largefile_local_inspection cloud_requests=0 source_ready=%t completed=%t stopped=%t deadline_expired=%t parts=%d api=%d requests=%d upload_reserved=%d upload_actual=%d read_reserved=%d read_actual=%d read_passes=%d read_inflight=%t detection_reserved=%d detection_actual=%d", st.SourceReady, st.Completed, st.Stopped != "", !time.Now().Before(st.Started.Add(largefileLifetime)), st.Put, st.API, st.Requests, st.UploadReserved, st.UploadActual, st.ReadReserved, st.ReadActual, st.ReadPasses, st.ReadInFlight, st.ReadDetectionReserved, st.ReadDetectionActual)
}
func runQuarkLargeFile(t *testing.T, preflightOnly bool) {
	agentDir, tvDir, parentDir, dir := os.Getenv("OMC_QUARK_LIVE_AGENT_STATE"), os.Getenv("OMC_QUARK_LIVE_TV_STATE"), os.Getenv("OMC_QUARK_LARGEFILE_PARENT"), os.Getenv("OMC_QUARK_LARGEFILE_STATE")
	if agentDir == "" || tvDir == "" || parentDir == "" || dir == "" {
		t.Skip("explicit completed donor and independent largefile batch required")
	}
	profile := os.Getenv("OMC_QUARK_LARGEFILE_PROFILE")
	if profile == "" {
		profile = largefileStandardProfile
	}
	if largefileValidateLiveProfile(profile, dir) != nil {
		t.Fatal("largefile_fixed_profile_directory_required")
	}
	check := func(stage string, e error) {
		t.Helper()
		if e != nil {
			t.Fatalf("largefile_stage=%s outcome=stopped_preserve_original_batch", stage)
		}
	}
	for _, a := range []string{agentDir, tvDir, parentDir} {
		if observationCheckPrivateDirectory(a) != nil || observationCheckPrivateDirectory(dir) != nil || !largefilePathScopesIndependent(a, dir) {
			t.Fatal("independent_nonoverlapping_private_batch_required")
		}
	}
	agent, e := openObservationAgent(agentDir)
	check("original_agent", e)
	defer agent.lock.Close()
	defer func() {
		if agent.unchanged() != nil {
			t.Error("original_agent_changed")
		}
	}()
	tv, e := openLiveTVStore(tvDir)
	check("original_tv", e)
	defer tv.lock.Close()
	defer func() {
		if tv.unchanged() != nil {
			t.Error("original_tv_changed")
		}
	}()
	if tv.checkpoint != nil || tv.attempt != nil || !tv.session.IdentityVerified {
		t.Fatal("original_tv_requires_separate_recovery")
	}
	parent, e := openFollowParent(parentDir, agent, tv)
	check("completed_donor", e)
	defer parent.store.lock.Close()
	defer func() {
		if parent.unchanged() != nil {
			t.Error("completed_donor_changed")
		}
	}()
	s, e := openLargeFileStore(dir, parent, profile)
	check("new_batch", e)
	defer s.lock.Close()
	size := largefileStateSize(s.state)
	if s.state.Completed {
		t.Log("largefile_completed=true cloud_requests=0")
		return
	}
	if s.state.Stopped != "" {
		t.Fatal("largefile_not_exercised=true cloud_requests=0 preserve_batch=true")
	}
	if s.state.ReadInFlight {
		t.Fatal("largefile_read_interrupted=true preserve_original_receipt=true cloud_requests=0")
	}
	if !time.Now().Before(s.state.Started.Add(largefileLifetime)) {
		t.Fatal("largefile_deadline_expired=true cloud_requests=0")
	}
	ctx, cancel := context.WithDeadline(context.Background(), s.state.Started.Add(largefileLifetime))
	defer cancel()
	s.cancel = cancel
	b := tv.accessOnly()
	b.Agent, e = agentRecoveryAccessSnapshot(agentDir, agent)
	check("current_agent_access", e)
	b.Proofs = append([]quark.SpaceProof(nil), parent.store.state.Bundle.Proofs...)
	b.Revision = s.state.Bundle.Revision + 1
	if b.TV == nil || b.Web != nil || b.TV.RefreshToken != "" || b.TV.AccountID != s.state.Identity.TVAccount || b.TV.DeviceID != s.state.Identity.TVDevice || !b.HasProof(s.state.Identity.Proof.TVRoot, s.state.Identity.Proof.AgentRoot) {
		t.Fatal("access_identity_changed")
	}
	s.state.Bundle = b
	check("access_snapshot", s.save())
	g := newLargeFileGuard(s)
	g.progress = func(stage string) {
		st := s.state
		t.Logf("largefile_progress stage=%s parts=%d api=%d requests=%d upload_bytes=%d read_bytes=%d read_passes=%d total_file_bytes=%d", stage, st.Put, st.API, st.Requests, st.UploadActual, st.ReadActual, st.ReadPasses, size)
	}
	defer func() {
		st := s.state
		t.Logf("requests=%d api=%d upload_pre=%d part_puts=%d upload_finish=%d mkdir=%d original_part_queries=%d media_reads=%d upload_reserved=%d upload_actual=%d read_reserved=%d read_actual=%d metadata_reserved=%d metadata_budget=%d last_kind=%s last_http=%d", st.Requests, st.API, st.Pre, st.Put, st.Finish, st.Mkdir, st.Queries, st.Reads, st.UploadReserved, st.UploadActual, st.ReadReserved, st.ReadActual, st.Metadata, largefileMetadataBudget(st), g.lastKind, g.lastStatus)
	}()
	auth := quark.NewObservationValidationAuthenticator(cloud.Config{}, g.wrap)
	check("tv_identity", auth.Verify(ctx, quark.RouteTV, b))
	check("agent_identity", auth.Verify(ctx, quark.RouteAgent, b))
	if preflightOnly {
		check("original_agent_unchanged", agent.unchanged())
		check("original_tv_unchanged", tv.unchanged())
		check("completed_donor_unchanged", parent.unchanged())
		t.Log("largefile_preflight_passed=true sample_generation=false cloud_writes=0")
		return
	}
	check("immutable_source_prepare", s.prepareSource(ctx))
	raw, _ := json.Marshal(b)
	d, e := quark.NewObservationValidationClient(cloud.Config{Credentials: raw, Operations: s}, g.wrap)
	check("native_client", e)
	check("owned_root", d.ValidateObservationOwnedRoot(ctx, s.state.Identity.ProofRootACK, strings.TrimPrefix(s.state.Identity.Proof.Path, "/")))
	s.state.RootVerified = true
	check("owned_root_checkpoint", s.save())
	original, e := d.ResolveDirectory(ctx, s.state.Identity.Proof.Path)
	check("proof_root", e)
	if original.ID != s.state.Identity.Proof.TVRoot || !original.IsDir {
		t.Fatal("proof_root_changed")
	}
	dirctx := quark.WithOperationID(ctx, s.state.DirectoryID)
	child, e := d.CreateDirectory(dirctx, s.state.Identity.Proof.AgentRoot, s.state.DirectoryName)
	check("mkdir", e)
	tvChild, e := d.ResolveReadItem(dirctx, child.ID)
	check("mkdir_mapping", e)
	if !tvChild.IsDir || tvChild.ParentID != original.ID || tvChild.Name != s.state.DirectoryName {
		t.Fatal("child_identity_changed")
	}
	managed, e := d.ResolveManagementPath(ctx, s.state.Identity.Proof.TVRoot, s.state.Identity.Proof.AgentRoot, "/"+s.state.DirectoryName)
	check("child_management", e)
	ag, ok := followRawID(managed.ID, quark.RouteAgent, s.state.Identity.AgentAccount)
	if !ok || !managed.IsDir {
		t.Fatal("child_management_invalid")
	}
	tvID, ok := followRawID(tvChild.ID, quark.RouteTV, s.state.Identity.TVAccount)
	if !ok {
		t.Fatal("child_tv_invalid")
	}
	if s.state.ChildAgent != "" && !quark.ObservationSameAgentHandle(s.state.ChildAgent, ag) || s.state.ChildTV != "" && s.state.ChildTV != tvID {
		t.Fatal("child_changed_on_resume")
	}
	s.state.ChildAgent, s.state.ChildTV = ag, tvID
	s.state.AgentIDs[ag] = true
	s.state.AgentParents[ag] = true
	s.state.TVParents[tvID] = true
	check("child_checkpoint", s.save())
	source, e := s.source()
	check("immutable_source", e)
	defer source.Close()
	upctx := quark.WithOperationID(ctx, s.state.UploadID)
	item, e := d.Upload(upctx, cloud.UploadRequest{ParentID: managed.ID, Name: s.state.UploadName, Size: size, Reader: source})
	if s.state.Stopped != "" {
		t.Fatal("largefile_not_exercised=true accepted_pre_preserved=true")
	}
	check("largefile_upload", e)
	item, e = d.ResolveReadItem(upctx, item.ID)
	check("upload_tv_mapping", e)
	if item.ParentID != tvChild.ID || item.Name != s.state.UploadName || item.IsDir || item.Size != size {
		t.Fatal("uploaded_output_changed")
	}
	read, e := d.OpenRead(ctx, cloud.ReadRequest{FileID: item.ID})
	check("explicit_complete_readback", e)
	h := sha256.New()
	n, e := io.Copy(h, io.LimitReader(read.Body, size+1))
	read.Body.Close()
	check("readback_body", e)
	if n != size || hex.EncodeToString(h.Sum(nil)) != s.state.SourceSHA {
		t.Fatal("full_readback_mismatch")
	}
	p, ok := largefileDecode(s)
	count, eligible := largefilePlan(p)
	if !ok || !eligible || s.state.ReadPasses != 3 || s.state.ReadActual != 3*size || s.state.ReadReserved != 3*size || s.state.ReadInFlight || s.state.Put != count || s.state.UploadActual != size || s.state.UploadReserved != size || s.state.Pre != 1 || s.state.Finish != 1 || len(p.Parts) != count || s.state.Operations[s.state.UploadID].State != "confirmed" || s.state.Operations[s.state.DirectoryID].State != "confirmed" {
		t.Fatal("largefile_evidence_incomplete")
	}
	// Recreate the native client and query only its original confirmed receipt.
	before := s.state.Requests
	cold, e := quark.NewObservationValidationClient(cloud.Config{Credentials: raw, Operations: s}, g.wrap)
	check("cold_native_client", e)
	result, e := cold.QueryOperation(ctx, s.state.UploadID)
	check("cold_original_receipt", e)
	if !result.Complete || s.state.Requests != before {
		t.Fatal("cold_query_not_local")
	}
	check("original_agent_unchanged", agent.unchanged())
	check("original_tv_unchanged", tv.unchanged())
	check("completed_donor_unchanged", parent.unchanged())
	s.state.Completed = true
	check("completed_checkpoint", s.save())
	t.Logf("largefile_completed=true parts=%d sample_bytes=%d full_sha256_match=true complete_readbacks=3 cold_confirmed_query_requests=0", count, size)
}

func largefilePathScopesIndependent(a, b string) bool {
	if !filepath.IsAbs(a) || !filepath.IsAbs(b) {
		return false
	}
	a, b = filepath.Clean(a), filepath.Clean(b)
	va, vb := filepath.VolumeName(a), filepath.VolumeName(b)
	if va != "" && vb != "" && !strings.EqualFold(va, vb) {
		return true
	}
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		rel, err := filepath.Rel(pair[0], pair[1])
		if err != nil || rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return false
		}
	}
	return true
}
