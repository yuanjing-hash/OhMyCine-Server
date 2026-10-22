//go:build quark_live

package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

// This helper runs only on explicit invocation by the owning operator. It never
// creates another fixture, uploads a proof, or resets the completed parent.
func TestQuarkLiveObservationFollowon(t *testing.T) {
	agentDir, tvDir := os.Getenv("OMC_QUARK_LIVE_AGENT_STATE"), os.Getenv("OMC_QUARK_LIVE_TV_STATE")
	parentDir, batchDir := os.Getenv("OMC_QUARK_OBSERVATION_PARENT"), os.Getenv("OMC_QUARK_OBSERVATION_FOLLOWON")
	if agentDir == "" || tvDir == "" || parentDir == "" || batchDir == "" {
		t.Skip("explicit completed parent and independent fixed follow-on batch required")
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
		code, _ := cloud.ErrorInfo(e)
		t.Fatalf("stage=%s code=%s", stage, code)
	}
	for _, a := range []string{agentDir, tvDir, parentDir} {
		for _, pair := range [][2]string{{a, batchDir}, {batchDir, a}} {
			rel, e := filepath.Rel(pair[0], pair[1])
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
	parent, e := openFollowParent(parentDir, agent, tv)
	check("completed_parent", e)
	defer parent.store.lock.Close()
	defer func() {
		if parent.unchanged() != nil {
			t.Error("completed_parent_changed")
		}
	}()
	if os.Getenv("OMC_QUARK_OBSERVATION_PARENT_ONLY") == "1" {
		t.Log("completed_parent_validated=true cloud_requests=0 new_journal_created=false")
		return
	}
	if mode := os.Getenv("OMC_QUARK_OBSERVATION_FOLLOWON_DIAGNOSTIC"); mode != "" {
		if mode != "local" && mode != "query" {
			t.Fatal("invalid_diagnostic_mode")
		}
		s, err := openExistingFollowDiagnosticStore(batchDir, parent)
		check("existing_followon_diagnostic", err)
		defer s.lock.Close()
		report, err := followDiagnostic(s)
		check("local_diagnostic", err)
		out, _ := json.Marshal(report)
		t.Log("followon_local_diagnostic=" + string(out))
		if mode == "local" || report.DeadlineExpired || report.BudgetExhausted || report.Completed {
			t.Log("diagnostic_cloud_requests=0 journal_unchanged=true")
			return
		}
		check("original_rename_query", followQueryRenameDiagnostic(t, s))
		return
	}
	s, e := openFollowStore(batchDir, parent)
	check("followon_journal", e)
	defer s.lock.Close()
	if s.state.Completed {
		t.Log("already_completed=true cloud_requests=0")
		return
	}
	ctx, cancel := context.WithDeadline(context.Background(), s.state.Started.Add(12*time.Minute))
	defer cancel()
	g := newFollowGuard(s)
	defer func() {
		st := s.state
		t.Logf("requests=%d api=%d media_reads=%d reserved_read_bytes=%d startup_broker=%d rename_dispatched=%t mkdir_dispatched=%t move_dispatched=%t", st.Requests, st.API, st.Reads, st.ReadBytes, st.BrokerWrites, st.Writes[st.RenameID+":rename"], st.Writes[st.DirectoryID+":mkdir"], st.Writes[st.MoveID+":move"])
		t.Logf("last_endpoint=%s last_outcome=%s last_http=%d", g.lastEndpoint, g.lastOutcome, g.lastStatus)
	}()
	bundle, renewed, e := tv.ready(ctx, quark.NewObservationValidationAuthenticator(cloud.Config{}, g.wrap))
	check("tv_startup", e)
	g.startup = false
	if renewed {
		t.Log("startup_tv_recovered=true")
	}
	a, i := agent.session, s.state.Identity
	bundle.Agent = &quark.Authorization{AccessToken: a.Access, DeviceID: a.Device, ClientDeviceID: a.Client, AccountID: a.User, Generation: a.Generation, IdentityVerified: true}
	bundle.Revision = s.state.Bundle.Revision + 1
	bundle.Proofs = append([]quark.SpaceProof(nil), parent.store.state.Bundle.Proofs...)
	if bundle.TV == nil || bundle.TV.RefreshToken != "" || bundle.TV.AccountID != i.TVAccount || bundle.TV.DeviceID != i.TVDevice || !bundle.TV.IdentityVerified || bundle.Web != nil || !bundle.HasProof(i.Proof.TVRoot, i.Proof.AgentRoot) {
		t.Fatal("latest_access_snapshot_invalid")
	}
	s.state.Bundle = bundle
	check("access_checkpoint", s.save())
	check("agent_identity", quark.NewObservationValidationAuthenticator(cloud.Config{}, g.wrap).Verify(ctx, quark.RouteAgent, bundle))
	raw, e := s.credentials(ctx)
	check("access_snapshot", e)
	d, e := quark.NewObservationValidationClient(cloud.Config{Credentials: raw, Operations: s}, g.wrap)
	check("native_client", e)
	root, e := d.ResolveDirectory(ctx, i.RootPath)
	check("current_root", e)
	if !root.IsDir || root.ID != i.Root.ID || root.ParentID != i.Root.ParentID || root.Name != i.Root.Name {
		t.Fatal("original_root_identity_changed")
	}
	managed, e := d.ResolveManagementPath(ctx, i.Proof.TVRoot, i.Proof.AgentRoot, "/"+i.Root.Name)
	check("current_root_mapping", e)
	managedRaw, ok := followRawID(managed.ID, quark.RouteAgent, i.AgentAccount)
	if !ok || !managed.IsDir || !quark.ObservationSameAgentHandle(managedRaw, i.RootAgent) {
		t.Fatal("original_management_root_changed")
	}
	if !s.state.InitialContentVerified {
		_, e = followSampleAt(ctx, d, i, i.Root.ID, i.Sample.Name)
		check("original_sample", e)
		item, e := d.ResolveManagementPath(ctx, i.Proof.TVRoot, i.Proof.AgentRoot, "/"+i.Root.Name+"/"+i.Sample.Name)
		check("original_sample_mapping", e)
		id, ok := followRawID(item.ID, quark.RouteAgent, i.AgentAccount)
		if !ok || item.IsDir || item.Size != observationFixtureSize || !quark.ObservationSameAgentHandle(id, i.SampleAgent) {
			t.Fatal("original_sample_management_identity_changed")
		}
		check("original_sample_content", followReadSample(ctx, d, i.Sample.ID))
		s.state.InitialContentVerified = true
		check("original_content_checkpoint", s.save())
	}
	registry := cloud.NewRegistry()
	check("registry", registry.Register(cloud.ProviderQuark, func(c cloud.Config) (cloud.Driver, error) {
		c.Operations = s
		c.PersistCredentials = nil
		c.LoadCredentials = nil
		return quark.NewObservationValidationClient(c, g.wrap)
	}))
	serviceDir := filepath.Join(parentDir, "service")
	check("existing_service_directory", observationCheckPrivateDirectory(serviceDir))
	if info, e := os.Lstat(filepath.Join(serviceDir, "observation.db")); e != nil || !info.Mode().IsRegular() {
		t.Fatal("existing_service_database_required")
	}
	serviceInput := services.QuarkObservationMutationLiveInput{
		Base:  services.QuarkObservationLiveInput{StateDir: serviceDir, Credentials: raw, Registry: registry, Root: i.Root, RootPath: i.RootPath, Sample: i.Sample, BaseURL: "http://127.0.0.1:38591", RequestCount: s.count},
		RunID: s.state.RunID, RenamedName: s.state.RenameName, DirectoryName: s.state.DirectoryName,
		Mutate: func(c context.Context, stage string) error { return followMutate(c, d, s, stage) },
	}
	report, e := services.RunQuarkObservationMutationLiveValidation(ctx, serviceInput)
	out, _ := json.Marshal(report)
	t.Log("observation_followon_report=" + string(out))
	check("observation_followon_service", e)
	beforeReopen := s.count()
	reopened, e := services.RunQuarkObservationMutationLiveValidation(ctx, serviceInput)
	check("completed_service_reopen", e)
	if !reopened.CompletedReopen || reopened.Requests != 0 || reopened.ReadyNotifications != 0 || s.count() != beforeReopen {
		t.Fatal("completed_service_reopen_did_not_remain_local")
	}
	out, _ = json.Marshal(reopened)
	t.Log("observation_followon_reopen=" + string(out))
	check("parent_unchanged", parent.unchanged())
	check("agent_unchanged", agent.unchanged())
	s.state.Completed = true
	check("completion_checkpoint", s.save())
	t.Log("parent_journal_unchanged=true original_agent_unchanged=true followon_completed=true")
}

func followReadSample(ctx context.Context, d *quark.Client, id string) error {
	r, e := d.OpenRead(ctx, cloud.ReadRequest{FileID: id})
	if e != nil {
		return e
	}
	defer r.Body.Close()
	b, e := io.ReadAll(io.LimitReader(r.Body, observationFixtureSize+1))
	if e != nil || len(b) != observationFixtureSize || followDigest(b) != observationFixtureSHA || r.TotalSize != nil && *r.TotalSize != observationFixtureSize {
		return observationError()
	}
	return nil
}
func followSampleAt(ctx context.Context, d *quark.Client, i followIdentity, parent, name string) (cloud.Item, error) {
	p, e := d.List(ctx, parent, cloud.PageRequest{Limit: 100})
	if e != nil {
		return cloud.Item{}, e
	}
	if p.HasMore || p.Total == nil || int64(len(p.Items)) != *p.Total {
		return cloud.Item{}, observationError()
	}
	var found cloud.Item
	count := 0
	for _, f := range p.Items {
		if f.ID == i.Sample.ID || f.Name == name {
			if f.ID != i.Sample.ID || f.Name != name || f.ParentID != parent || f.IsDir || f.Size != observationFixtureSize {
				return cloud.Item{}, observationError()
			}
			found = f
			count++
		}
	}
	if count != 1 {
		return cloud.Item{}, observationError()
	}
	return found, nil
}
func followReceiptConfirmed(ctx context.Context, s *followStore, id string) error {
	r, e := s.Load(ctx, id)
	if e != nil || r.State != "confirmed" {
		return observationError()
	}
	return nil
}
func followMutate(ctx context.Context, d *quark.Client, s *followStore, stage string) error {
	i := s.state.Identity
	if !s.state.InitialContentVerified || s.state.Completed {
		return observationError()
	}
	sample := quark.QualifyID(quark.RouteAgent, i.AgentAccount, i.SampleAgent)
	switch stage {
	case "rename_one":
		if e := d.Rename(quark.WithOperationID(ctx, s.state.RenameID), sample, s.state.RenameName); e != nil {
			return e
		}
		if e := followReceiptConfirmed(ctx, s, s.state.RenameID); e != nil {
			return e
		}
		_, e := followSampleAt(ctx, d, i, i.Root.ID, s.state.RenameName)
		return e
	case "mkdir_move_one":
		if e := followReceiptConfirmed(ctx, s, s.state.RenameID); e != nil {
			return e
		}
		c := quark.WithOperationID(ctx, s.state.DirectoryID)
		child, e := d.CreateDirectory(c, quark.QualifyID(quark.RouteAgent, i.AgentAccount, i.RootAgent), s.state.DirectoryName)
		if e != nil {
			return e
		}
		if e = followReceiptConfirmed(ctx, s, s.state.DirectoryID); e != nil {
			return e
		}
		child, e = d.ResolveReadItem(c, child.ID)
		if e != nil {
			return e
		}
		if !child.IsDir || child.ParentID != i.Root.ID || child.Name != s.state.DirectoryName {
			return observationError()
		}
		managed, e := d.ResolveManagementPath(ctx, i.Proof.TVRoot, i.Proof.AgentRoot, "/"+i.Root.Name+"/"+s.state.DirectoryName)
		if e != nil {
			return e
		}
		raw, ok := followRawID(managed.ID, quark.RouteAgent, i.AgentAccount)
		if !ok || !managed.IsDir {
			return observationError()
		}
		if s.state.NewDirectory.ID != "" && (s.state.NewDirectory.ID != child.ID || !quark.ObservationSameAgentHandle(raw, s.state.NewDirectoryAgent)) {
			return observationError()
		}
		child.PickCode = ""
		s.state.NewDirectory = child
		s.state.NewDirectoryAgent = raw
		if e = s.save(); e != nil {
			return e
		}
		if e = d.Move(quark.WithOperationID(ctx, s.state.MoveID), sample, managed.ID); e != nil {
			return e
		}
		if e = followReceiptConfirmed(ctx, s, s.state.MoveID); e != nil {
			return e
		}
		if _, e = followSampleAt(ctx, d, i, child.ID, s.state.RenameName); e != nil {
			return e
		}
		return followReadSample(ctx, d, i.Sample.ID)
	default:
		return observationError()
	}
}
