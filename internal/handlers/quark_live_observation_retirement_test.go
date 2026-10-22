//go:build quark_live

package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

func TestQuarkLiveObservationRetirement(t *testing.T) {
	agentDir, tvDir := os.Getenv("OMC_QUARK_LIVE_AGENT_STATE"), os.Getenv("OMC_QUARK_LIVE_TV_STATE")
	creationDir, followDir, batchDir := os.Getenv("OMC_QUARK_RETIREMENT_CREATION"), os.Getenv("OMC_QUARK_RETIREMENT_PREDECESSOR"), os.Getenv("OMC_QUARK_RETIREMENT_STATE")
	if agentDir == "" || tvDir == "" || creationDir == "" || followDir == "" || batchDir == "" {
		t.Skip("explicit successful creation and follow-on predecessors required")
	}
	check := func(stage string, e error) {
		t.Helper()
		if e == nil {
			return
		}
		var local observationLocalFailure
		if errors.As(e, &local) {
			t.Fatalf("stage=%s code=%s", stage, local)
		}
		var failure *quark.Failure
		if errors.As(e, &failure) {
			t.Fatalf("stage=%s code=%s http=%d errno=%d", stage, failure.Code, failure.HTTPStatus, failure.Errno)
		}
		t.Fatalf("stage=%s code=validation_stopped", stage)
	}
	for _, source := range []string{agentDir, tvDir, creationDir, followDir} {
		for _, p := range [][2]string{{source, batchDir}, {batchDir, source}} {
			rel, e := filepath.Rel(p[0], p[1])
			if e != nil || rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Fatal("independent_nonoverlapping_batch_required")
			}
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
			t.Error("original_tv_key_changed")
		}
	}()
	creation, e := openFollowParent(creationDir, agent, tv)
	check("completed_creation", e)
	defer creation.store.lock.Close()
	defer func() {
		if creation.unchanged() != nil {
			t.Error("creation_changed")
		}
	}()
	parent, e := openRetirementParent(followDir, creation)
	check("completed_predecessor", e)
	defer parent.store.lock.Close()
	defer func() {
		if parent.unchanged() != nil {
			t.Error("predecessor_changed")
		}
	}()
	if os.Getenv("OMC_QUARK_RETIREMENT_PARENT_ONLY") == "1" {
		check("creation_unchanged", creation.unchanged())
		check("predecessor_unchanged", parent.unchanged())
		check("agent_unchanged", agent.unchanged())
		check("tv_key_unchanged", tv.unchanged())
		t.Log("completed_creation=true completed_predecessor=true original_state_unchanged=true cloud_requests=0 new_journal=false")
		return
	}
	s, e := openRetirementStore(batchDir, parent)
	check("retirement_journal", e)
	defer s.lock.Close()
	if s.state.Completed {
		t.Log("already_completed=true cloud_requests=0")
		return
	}
	ctx, cancel := context.WithDeadline(context.Background(), s.state.Deadline)
	defer cancel()
	g := newRetirementGuard(s)
	defer func() {
		st := s.state
		t.Logf("requests=%d api=%d startup_broker=%d move_out_dispatched=%t move_back_dispatched=%t media_reads=0", st.Requests, st.API, st.BrokerWrites, st.Writes[st.MoveOutID+":move"], st.Writes[st.MoveBackID+":move"])
		t.Logf("last_endpoint=%s last_outcome=%s last_http=%d", g.lastEndpoint, g.lastOutcome, g.lastStatus)
	}()
	bundle, renewed, e := tv.ready(ctx, quark.NewObservationValidationAuthenticator(cloud.Config{}, g.wrap))
	check("tv_startup", e)
	g.startup = false
	if renewed {
		t.Log("startup_tv_recovered=true")
	}
	a, c := agent.session, s.state.Identity.Creation
	bundle.Agent = &quark.Authorization{AccessToken: a.Access, DeviceID: a.Device, ClientDeviceID: a.Client, AccountID: a.User, Generation: a.Generation, IdentityVerified: true}
	bundle.Proofs = append([]quark.SpaceProof(nil), parent.store.state.Bundle.Proofs...)
	bundle.Revision = s.state.Bundle.Revision + 1
	if bundle.TV == nil || bundle.TV.AccountID != c.TVAccount || bundle.TV.DeviceID != c.TVDevice || !bundle.TV.IdentityVerified || bundle.TV.RefreshToken != "" || bundle.Agent.RefreshToken != "" || bundle.Web != nil || !bundle.HasProof(c.Proof.TVRoot, c.Proof.AgentRoot) {
		t.Fatal("access_snapshot_identity_mismatch")
	}
	s.state.Bundle = bundle
	check("access_checkpoint", s.save())
	check("agent_identity", quark.NewObservationValidationAuthenticator(cloud.Config{}, g.wrap).Verify(ctx, quark.RouteAgent, bundle))
	raw, e := s.credentials(ctx)
	check("access_snapshot", e)
	d, e := quark.NewObservationValidationClient(cloud.Config{Credentials: raw, Operations: s}, g.wrap)
	check("native_client", e)
	registry := cloud.NewRegistry()
	check("registry", registry.Register(cloud.ProviderQuark, func(config cloud.Config) (cloud.Driver, error) {
		config.Operations = s
		config.LoadCredentials = nil
		config.PersistCredentials = nil
		return quark.NewObservationValidationClient(config, g.wrap)
	}))
	i := s.state.Identity
	var lastProgress time.Time
	in := services.QuarkObservationRetirementLiveInput{
		Base:  services.QuarkObservationLiveInput{StateDir: filepath.Join(creationDir, "service"), Credentials: raw, Registry: registry, Root: c.Root, RootPath: c.RootPath, Sample: c.Sample, BaseURL: "http://127.0.0.1:38591", RequestCount: s.count},
		RunID: s.state.RunID, StartedAt: s.state.Started, Deadline: s.state.Deadline,
		Predecessor: services.QuarkObservationRetirementPredecessor{RunID: i.PredecessorRunID, RenamedName: i.Name, DirectoryName: i.DirectoryName, DirectoryTVID: i.Inside.ID, JournalSHA256: i.PredecessorJournalSHA, KeySHA256: i.PredecessorKeySHA},
		Mutate:      func(call context.Context, stage string) error { return retirementMutate(call, d, s, stage) },
		Progress: func(p services.QuarkObservationRetirementProgress) {
			if !lastProgress.IsZero() && time.Since(lastProgress) < 30*time.Second {
				return
			}
			lastProgress = time.Now()
			stage := "validation"
			switch p.Stage {
			case "manual_match", "manual_edit", "manual_artifacts", "user_state", "move_out", "first_observe", "wait_confirm", "second_observe", "revoked_verify", "revoked_reopen", "retire_artifacts", "move_back", "restore_observe", "restore_artifacts", "late_cleanup", "noop_observe", "noop_artifacts", "completed_reopen", "completed":
				stage = p.Stage
			}
			wait := p.WaitSeconds
			if wait < 0 || wait > 1500 {
				wait = 0
			}
			t.Logf("retirement_stage=%s wait_seconds=%d", stage, wait)
		},
	}
	report, e := services.RunQuarkObservationRetirementLiveValidation(ctx, in)
	output, _ := json.Marshal(report)
	t.Log("observation_retirement_report=" + string(output))
	check("retirement_service", e)
	if !report.Completed || !report.CompletedDBReopen {
		t.Fatal("retirement_completion_unverified")
	}
	before := s.count()
	calls := 0
	in.Mutate = func(context.Context, string) error { calls++; return observationError() }
	reopened, e := services.RunQuarkObservationRetirementLiveValidation(ctx, in)
	check("completed_service_reopen", e)
	if !reopened.Completed || !reopened.CompletedDBReopen || reopened.Requests != 0 || s.count() != before || calls != 0 {
		t.Fatal("completed_service_reopen_not_local")
	}
	output, _ = json.Marshal(reopened)
	t.Log("observation_retirement_reopen=" + string(output))
	check("creation_unchanged", creation.unchanged())
	check("predecessor_unchanged", parent.unchanged())
	check("agent_unchanged", agent.unchanged())
	s.state.Completed = true
	check("completion_checkpoint", s.save())
	t.Log("retirement_completed=true original_journals_unchanged=true")
}
