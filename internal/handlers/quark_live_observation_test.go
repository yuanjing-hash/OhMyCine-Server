//go:build quark_live

package handlers_test

import (
	"bytes"
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

func TestQuarkLiveObservation(t *testing.T) {
	agentDir, tvDir, batchDir, fixturePath := os.Getenv("OMC_QUARK_LIVE_AGENT_STATE"), os.Getenv("OMC_QUARK_LIVE_TV_STATE"), os.Getenv("OMC_QUARK_OBSERVATION_STATE"), os.Getenv("OMC_QUARK_OBSERVATION_FIXTURE")
	if agentDir == "" || tvDir == "" || batchDir == "" || fixturePath == "" {
		t.Skip("explicit existing authorization and fresh observation batch required")
	}
	check := func(stage string, e error) {
		t.Helper()
		if e != nil {
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
	}
	for _, original := range []string{agentDir, tvDir} {
		rel, e := filepath.Rel(original, batchDir)
		if e != nil || rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatal("new independent batch directory required")
		}
	}
	content, e := observationFixture(fixturePath)
	check("fixture", e)
	// Fixed order: original Agent lock, original TV lock, independent batch lock.
	agent, e := openObservationAgent(agentDir)
	check("original_agent_state", e)
	defer agent.lock.Close()
	defer func() {
		if agent.unchanged() != nil {
			t.Error("original_agent_state_changed")
		}
	}()
	tv, e := openLiveTVStore(tvDir)
	check("original_tv_state", e)
	defer tv.lock.Close()
	defer func() {
		if tv.unchanged() != nil {
			t.Error("original_tv_state_changed")
		}
	}()
	store, e := openObservationStore(batchDir, agent, tv)
	check("durable_batch", e)
	defer store.lock.Close()
	if store.state.Completed {
		t.Log("already_completed=true cloud_requests=0")
		return
	}
	ctx, cancel := context.WithDeadline(context.Background(), store.state.Started.Add(12*time.Minute))
	defer cancel()
	g := newObservationGuard(store)
	defer func() {
		s := store.state
		t.Logf("requests=%d api=%d media_reads=%d reserved_read_bytes=%d upload_pre=%d upload_put=%d upload_finish=%d mkdir=%d startup_broker=%d", s.Requests, s.API, s.Reads, s.ReadBytes, s.PreWrites, s.PutWrites, s.FinishWrites, s.DirectoryWrites, s.BrokerWrites)
		t.Logf("last_endpoint=%s last_outcome=%s last_http=%d", g.lastEndpoint, g.lastOutcome, g.lastStatus)
	}()
	bundle, renewed, e := tv.ready(ctx, quark.NewObservationValidationAuthenticator(cloud.Config{}, g.wrap))
	check("tv_startup", e)
	g.startup = false
	if renewed {
		t.Log("startup_tv_recovered=true")
	}
	a := agent.session
	bundle.Agent = &quark.Authorization{AccessToken: a.Access, DeviceID: a.Device, ClientDeviceID: a.Client, AccountID: a.User, Generation: a.Generation, IdentityVerified: true}
	bundle.Revision = store.state.Bundle.Revision + 1
	bundle.Proofs = store.state.Bundle.Proofs
	if bundle.TV == nil || bundle.TV.RefreshToken != "" || bundle.TV.AccountID != store.state.TVAccount {
		t.Fatal("latest_access_snapshot_invalid")
	}
	store.state.Bundle = bundle
	check("latest_access_checkpoint", store.save())
	check("agent_identity", quark.NewObservationValidationAuthenticator(cloud.Config{}, g.wrap).Verify(ctx, quark.RouteAgent, bundle))
	client := func() *quark.Client {
		raw, e := store.credentials(ctx)
		check("access_snapshot", e)
		d, e := quark.NewObservationValidationClient(cloud.Config{Credentials: raw, Operations: store, PersistCredentials: store.persist, LoadCredentials: store.credentials}, g.wrap)
		check("native_driver", e)
		return d
	}
	d := client()
	check("owned_root_receipt", d.ValidateObservationOwnedRoot(ctx, store.state.RootACK, store.state.RootName))
	store.state.RootVerified = true
	check("owned_root_checkpoint", store.save())
	proof, e := d.VerifyCommonSpace(ctx, store.state.ProofID, "/"+store.state.RootName)
	check("common_space", e)
	d = client()
	dirctx := quark.WithOperationID(ctx, store.state.DirectoryID)
	child, e := d.CreateDirectory(dirctx, proof.AgentRoot, store.state.DirectoryName)
	check("mkdir", e)
	root, e := d.ResolveReadItem(dirctx, child.ID)
	check("mkdir_mapping", e)
	managed, e := d.ResolveManagementPath(ctx, proof.TVRoot, proof.AgentRoot, "/"+store.state.DirectoryName)
	check("child_management_mapping", e)
	upctx := quark.WithOperationID(ctx, store.state.UploadID)
	sample, e := d.Upload(upctx, cloud.UploadRequest{ParentID: managed.ID, Name: store.state.UploadName, Size: int64(len(content)), Reader: bytes.NewReader(content)})
	check("synthetic_upload", e)
	sample, e = d.ResolveReadItem(upctx, sample.ID)
	check("synthetic_mapping", e)
	if !root.IsDir || sample.IsDir || sample.ParentID != root.ID || sample.Name != store.state.UploadName || sample.Size != observationFixtureSize {
		t.Fatal("confirmed_fixture_scope_mismatch")
	}
	for _, id := range []string{store.state.ProofID, store.state.ProofID + "/upload", store.state.DirectoryID, store.state.UploadID} {
		r, e := store.Load(ctx, id)
		check("receipt", e)
		if r.State != "confirmed" {
			t.Fatal("unconfirmed_cloud_output")
		}
	}
	// The service assembler uses fresh real clients, preserving its own normal
	// credentials/fences. It does not get a new way to write or refresh.
	registry := cloud.NewRegistry()
	check("registry", registry.Register(cloud.ProviderQuark, func(c cloud.Config) (cloud.Driver, error) {
		c.Operations = store
		c.PersistCredentials = nil
		c.LoadCredentials = nil
		return quark.NewObservationValidationClient(c, g.wrap)
	}))
	raw, e := store.credentials(ctx)
	check("service_access_snapshot", e)
	serviceDir := filepath.Join(batchDir, "service")
	if e = os.Mkdir(serviceDir, 0700); e != nil {
		t.Fatal("service_directory_exists_or_unavailable_preserved")
	}
	check("service_directory", observationCheckPrivateDirectory(serviceDir))
	report, e := services.RunQuarkObservationLiveValidation(ctx, services.QuarkObservationLiveInput{StateDir: serviceDir, Credentials: raw, Registry: registry, Root: root, RootPath: "/" + store.state.RootName + "/" + store.state.DirectoryName, Sample: sample, BaseURL: "http://127.0.0.1:38591", RequestCount: store.count})
	out, _ := json.Marshal(report)
	t.Log("observation_report=" + string(out))
	check("observation_service", e)
	store.state.Completed = true
	check("completion_checkpoint", store.save())
	t.Log("original_agent_files_unchanged=true existing_tv_key_preserved=true new_batch_completed=true")
}
