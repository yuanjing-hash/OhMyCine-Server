//go:build quark_live && windows

package handlers_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

func TestQuarkLiveNodeProbe(t *testing.T)          { runLiveNodeProbe(t, false) }
func TestQuarkLiveNodeProbePreflight(t *testing.T) { runLiveNodeProbe(t, true) }
func runLiveNodeProbe(t *testing.T, preflight bool) {
	if os.Getenv("OMC_QUARK_NODE_PROBE") != "1" {
		t.Skip("explicit isolated Node validation required")
	}
	root := filepath.FromSlash(nodeProbeDirectory)
	if os.Getenv("OMC_QUARK_NODE_PROBE_STATE") != root || observationCheckPrivateDirectory(root) != nil {
		t.Fatal("node_fixed_private_batch_required")
	}
	agentDir, tvDir, parentDir := os.Getenv("OMC_QUARK_LIVE_AGENT_STATE"), os.Getenv("OMC_QUARK_LIVE_TV_STATE"), os.Getenv("OMC_QUARK_NODE_PROBE_PARENT")
	for _, dir := range []string{agentDir, tvDir, parentDir} {
		if dir == "" || observationCheckPrivateDirectory(dir) != nil || !largefilePathScopesIndependent(root, dir) {
			t.Fatal("node_independent_original_paths_required")
		}
	}
	agent, e := openObservationAgent(agentDir)
	if e != nil {
		t.Fatal("node_original_agent_unavailable")
	}
	defer agent.lock.Close()
	defer func() {
		if agent.unchanged() != nil {
			t.Error("node_original_agent_changed")
		}
	}()
	tv, e := openLiveTVStore(tvDir)
	if e != nil {
		t.Fatal("node_original_tv_unavailable")
	}
	defer tv.lock.Close()
	defer func() {
		if tv.unchanged() != nil {
			t.Error("node_original_tv_changed")
		}
	}()
	if tv.checkpoint != nil || tv.attempt != nil {
		t.Fatal("node_tv_requires_separate_recovery")
	}
	parent, e := openFollowParent(parentDir, agent, tv)
	if e != nil {
		t.Fatal("node_completed_donor_rejected")
	}
	defer parent.store.lock.Close()
	defer func() {
		if parent.unchanged() != nil {
			t.Error("node_completed_donor_changed")
		}
	}()
	store, c, e := nodeProbePrepareController(root, parent.identity)
	if e != nil {
		t.Fatal("node_controller_state_rejected")
	}
	defer store.lock.Close()
	b, e := nodeProbeAccessBundle(agentDir, agent, tv, parent)
	if e != nil {
		t.Fatal("node_parent_access_snapshot_unavailable")
	}
	if !c.Completed {
		g, e := openNodeProbeGuard(root, c.Spec)
		if e != nil {
			t.Fatal("node_guard_state_rejected")
		}
		raw, _ := json.Marshal(b)
		if g.bind(cloud.Config{Credentials: raw}) != nil {
			g.journal.lock.Close()
			t.Fatal("node_access_scope_rejected")
		}
		ctx, cancel := context.WithDeadline(context.Background(), c.Spec.Started.Add(nodeProbeLifetime))
		auth := quark.NewObservationValidationAuthenticator(cloud.Config{}, g.wrap)
		for _, route := range []quark.Route{quark.RouteTV, quark.RouteAgent} {
			if e = auth.Verify(ctx, route, b); e != nil {
				cancel()
				g.journal.lock.Close()
				t.Logf("node_preflight route=%s native_code=%s requests=%d api=%d cloud_writes=0", route, largefileRefreshCode(e), g.state.Requests, g.state.API)
				t.Fatal("node_identity_preflight_failed")
			}
		}
		cancel()
		g.journal.lock.Close()
	}
	if preflight {
		t.Log("node_preflight_passed=true sample_bytes=8388609 cloud_writes=0 original_credentials_unchanged=true")
		return
	}
	executeNodeProbe(t, root, store, c, b, false)
}

// Parent-side adapter only. The Node receives this access-only bundle through
// its real sealed grant. Any same-identity recovery sidecar is applied here,
// never by exposing original credential paths to the child process.
func nodeProbeAccessBundle(agentDir string, a *observationAgentState, tv *liveTVStore, parent *followParent) (quark.Credentials, error) {
	b := tv.accessOnly()
	access, e := agentRecoveryAccessSnapshot(agentDir, a)
	if e != nil {
		return quark.Credentials{}, e
	}
	b.Agent = access
	b.Proofs = []quark.SpaceProof{parent.identity.Proof}
	b.Revision = 1
	return b, nil
}
