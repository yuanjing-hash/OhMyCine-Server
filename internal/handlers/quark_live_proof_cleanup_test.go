//go:build quark_live

package handlers_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

func TestQuarkLiveProofCleanup(t *testing.T) {
	if os.Getenv("OMC_QUARK_PROOF_CLEANUP") != "1" {
		t.Skip("explicit isolated 32-byte proof cleanup acceptance required")
	}
	agentDir, tvDir, donorDir, batchDir := os.Getenv("OMC_QUARK_LIVE_AGENT_STATE"), os.Getenv("OMC_QUARK_LIVE_TV_STATE"), os.Getenv("OMC_QUARK_PROOF_DONOR_STATE"), os.Getenv("OMC_QUARK_PROOF_CLEANUP_STATE")
	check := func(stage string, err error) {
		t.Helper()
		if err == nil {
			return
		}
		var phase *quark.OperationError
		if errors.As(err, &phase) {
			t.Logf("native_phase=%s", phase.Stage)
		}
		if state, ok := quark.UploadDiagnosticFromError(err); ok {
			t.Logf("upload_phase=%s upload_state=%s has_task=%t has_output=%t target_recorded=%t not_dispatched=%t inflight_part=%d confirmed_parts=%d task_pages=%d task_listing_complete=%t", state.Phase, state.State, state.HasTask, state.HasOutput, state.TargetRecorded, state.NotDispatched, state.InFlightPart, state.ConfirmedParts, state.TaskPages, state.TaskListingComplete)
			if state.TaskFinishStatus != nil {
				t.Logf("task_finish_status=%d", *state.TaskFinishStatus)
			}
		}
		var failure *quark.Failure
		if errors.As(err, &failure) {
			t.Fatalf("stage=%s code=%s http=%d errno=%d batch_preserved=true", stage, failure.Code, failure.HTTPStatus, failure.Errno)
		}
		code, _ := cloud.ErrorInfo(err)
		t.Fatalf("stage=%s code=%s batch_preserved=true", stage, code)
	}
	paths := []string{agentDir, tvDir, donorDir, batchDir}
	for n, a := range paths {
		if !filepath.IsAbs(a) || observationCheckPrivateDirectory(a) != nil {
			t.Fatal("explicit existing private state directories required")
		}
		for _, b := range paths[n+1:] {
			for _, pair := range [][2]string{{a, b}, {b, a}} {
				rel, err := filepath.Rel(pair[0], pair[1])
				if err != nil || rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					t.Fatal("independent fixed batch required")
				}
			}
		}
	}
	// Same lock order as the observation harness. The original sources remain
	// read-only; access-only clients have no refresh operation in their guard.
	agent, err := openObservationAgent(agentDir)
	check("agent_source", err)
	defer agent.lock.Close()
	defer func() {
		if agent.unchanged() != nil {
			t.Error("agent_source_changed")
		}
	}()
	continuation, err := liveTVRead(filepath.Join(agentDir, agentRecoveryName), 1<<20, false)
	check("existing_agent_continuation", err)
	a, err := agentRecoveryAccessSnapshot(agentDir, agent)
	check("fresh_agent_access_snapshot", err)
	agent.files[filepath.Join(agentDir, agentRecoveryName)] = continuation
	tv, err := openLiveTVStore(tvDir)
	check("tv_source", err)
	defer tv.lock.Close()
	defer func() {
		if tv.unchanged() != nil {
			t.Error("tv_source_changed")
		}
	}()
	if tv.attempt != nil || tv.checkpoint != nil {
		t.Fatal("tv_pending_recovery_preserved_no_refresh_permitted")
	}
	donor, err := openObservationStoreReadOnly(donorDir, agent, tv)
	check("completed_donor_binding", err)
	defer donor.lock.Close()
	// Capture immutable donor bytes while its own lock is already held, so
	// the identity digest and after-run check describe the same generation.
	journal, err := liveTVRead(filepath.Join(donorDir, "observation.enc"), 2<<20, false)
	check("completed_donor_journal", err)
	key, err := liveTVRead(filepath.Join(donorDir, "observation.key"), 1024, false)
	check("completed_donor_key", err)
	defer func() {
		for name, original := range map[string][]byte{"observation.enc": journal, "observation.key": key} {
			current, e := liveTVRead(filepath.Join(donorDir, name), 2<<20, false)
			if e != nil || !bytes.Equal(original, current) {
				t.Error("completed_donor_changed")
			}
		}
	}()
	identity, err := proofCleanupDonor(donor.state, journal, key)
	check("completed_donor_root_proof", err)
	access := tv.accessOnly()
	access.Agent, access.Proofs = a, nil
	st := donor.state
	if access.TV.DeviceID != st.TVDevice || access.TV.AccountID != st.TVAccount || a.AccountID != st.AgentAccount || a.DeviceID != st.AgentDevice || a.ClientDeviceID != st.AgentClient || a.Generation < st.AgentGeneration {
		t.Fatal("access_snapshot_donor_identity_mismatch")
	}
	s, err := openProofCleanupStore(batchDir, identity, access)
	check("fixed_proof_cleanup_journal", err)
	defer s.lock.Close()
	if s.state.Completed {
		t.Log("already_completed=true cloud_requests=0 original_verification_file_absent_from_root=true")
		return
	}
	ctx, cancel := context.WithDeadline(context.Background(), s.state.Deadline)
	defer cancel()
	g := newProofCleanupGuard(s)
	defer func() {
		st := s.state
		t.Logf("requests=%d api=%d reserved_read_bytes=%d metadata_bytes=%d pre=%t put=%t finish=%t delete=%t completed=%t", st.Requests, st.API, st.ReadBytes, st.MetadataBytes, st.Writes["pre"], st.Writes["put"], st.Writes["finish"], st.Writes["delete"], st.Completed)
		t.Logf("last_endpoint=%s last_outcome=%s last_http=%d last_errno=%d", g.lastEndpoint, g.lastOutcome, g.lastStatus, g.lastErrno)
	}()
	auth := quark.NewObservationValidationAuthenticator(cloud.Config{}, g.wrap)
	check("agent_identity_no_refresh", auth.Verify(ctx, quark.RouteAgent, s.state.Bundle))
	check("tv_identity_no_refresh", auth.Verify(ctx, quark.RouteTV, s.state.Bundle))
	client := func() *quark.Client {
		raw, err := s.credentials(ctx)
		check("private_snapshot", err)
		d, err := quark.NewObservationValidationClient(cloud.Config{Credentials: raw, Operations: s, PersistCredentials: s.persist, LoadCredentials: s.credentials}, g.wrap)
		check("native_client", err)
		return d
	}
	d := client()
	check("frozen_owned_experiment_root", d.ValidateObservationOwnedRoot(ctx, identity.RootACK, identity.RootName))
	s.state.RootVerified = true
	check("owned_root_checkpoint", s.save())
	proof, err := d.VerifyCommonSpace(ctx, s.state.ProofID, "/"+identity.RootName)
	check("common_space_upload_readback_save", err)
	if !s.state.Bundle.HasProof(proof.TVRoot, proof.AgentRoot) {
		t.Fatal("proof_not_durably_saved")
	}
	// A fresh client exercises the persisted proof and cleanup receipts. A
	// failed/unknown deletion may only reconcile its original target here.
	d = client()
	cleanup, err := d.CleanupCommonSpace(ctx, s.state.ProofID)
	if err != nil || cleanup != "complete" {
		t.Log("proof_saved=true cleanup_pending=true")
		check("exact_cleanup", err)
		t.Fatal("cleanup_pending_batch_preserved")
	}
	for _, id := range []string{s.state.ProofID, s.state.ProofID + "/upload"} {
		r, err := s.Load(ctx, id)
		check("confirmed_receipt", err)
		if r.State != "confirmed" {
			t.Fatal("unconfirmed_receipt")
		}
	}
	s.state.Completed = true
	check("completion_checkpoint", s.save())
	t.Log("proof_saved=true independent_tv_full_readback=true original_verification_file_absent_from_root=true permanent_purge_not_asserted=true original_sources_unchanged=true")
}
