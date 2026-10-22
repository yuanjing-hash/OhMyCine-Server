package nodeagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

func quarkTestGrantState(t *testing.T, a *Agent, key string, c nodeprotocol.StorageCredential) *quarkGrantState {
	t.Helper()
	now := a.now()
	ctx := context.Background()
	op, err := a.store.Get(ctx, "server-1", key)
	if errors.Is(err, ErrOperationNotFound) {
		plan := nodeprotocol.OperationPlan{ProtocolVersion: 1, OperationKey: key, TaskID: "task-1", NodeID: "node-1", Kind: nodeprotocol.OperationKindStorageUpload, PlanRevision: 1, LeaseEpoch: 1, LeaseExpiresAt: now.Add(time.Hour), Payload: json.RawMessage(`{"fixture":true}`)}
		digest, digestErr := plan.Digest()
		if digestErr != nil {
			t.Fatal(digestErr)
		}
		op, _, err = a.store.Put(ctx, "server-1", digest, plan, now)
	}
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := validateQuarkGrant(c)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	grant := nodeprotocol.CredentialGrant{GrantID: "grant-old", NodeID: "node-1", TaskID: "task-1", OperationKey: key, ResourceKind: nodeprotocol.ResourceKindStorage, ResourceID: "storage-1", CredentialRevision: bundle.Revision, AllowedActions: []string{nodeprotocol.StorageActionUpload}, ExpiresAt: now.Add(time.Minute), Credential: raw}
	return a.newQuarkGrantState("server-1", op.PlanDigest, grant, c)
}

func quarkGrantFixture(t *testing.T) (*Agent, *quarkGrantState, quark.Credentials) {
	t.Helper()
	a := newQuarkNodeTestAgent(t)
	tv := &quark.Authorization{AccessToken: "old-tv", DeviceID: "tv-device", AccountID: "tv-account", IdentityVerified: true, Generation: 1}
	agent := &quark.Authorization{AccessToken: "old-agent", DeviceID: "agent-device", ClientDeviceID: "agent-client", AccountID: "agent-account", IdentityVerified: true, Generation: 1}
	proof := quark.SpaceProof{Version: 1, Generation: 1, TVAccount: tv.AccountID, AgentAccount: agent.AccountID, TVRoot: quark.QualifyID(quark.RouteTV, tv.AccountID, "T"), AgentRoot: quark.QualifyID(quark.RouteAgent, agent.AccountID, "A"), Path: "/library", ContentSHA256: strings.Repeat("a", 64), VerifiedAt: a.now()}
	b := quark.Credentials{Version: 1, Revision: 2, TV: tv, Agent: agent, Proofs: []quark.SpaceProof{proof}}
	raw, _ := json.Marshal(b)
	c := nodeprotocol.StorageCredential{ProviderType: cloud.ProviderQuark, RootID: proof.TVRoot, Quark: raw}
	return a, quarkTestGrantState(t, a, "operation-1", c), b
}

func saveNextQuarkGrant(t *testing.T, g *quarkGrantState, bundle quark.Credentials, mutate func(*nodeprotocol.CredentialGrant, *nodeprotocol.StorageCredential)) {
	t.Helper()
	next := g.grant
	next.GrantID = "grant-new"
	next.CredentialRevision = bundle.Revision
	next.ExpiresAt = g.agent.now().Add(10 * time.Minute)
	c := g.credential
	c.Quark, _ = json.Marshal(bundle)
	if mutate != nil {
		mutate(&next, &c)
	}
	next.Credential, _ = json.Marshal(c)
	sealed, err := nodeprotocol.SealCredentialGrant(g.agent.sealingPublicKey, next, g.agent.now())
	if err != nil {
		t.Fatal(err)
	}
	if err = g.agent.store.SaveCredentialGrant(context.Background(), g.serverID, sealed, g.agent.now()); err != nil {
		t.Fatal(err)
	}
}

func TestQuarkGrantLoaderAdoptsOnlySameOperationScope(t *testing.T) {
	for _, mode := range []string{"renew", "same-revision", "extra-action", "missing-action", "client-device", "generation", "proof", "revision-envelope", "revision-rollback", "refresh", "web", "root", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			a, g, b := quarkGrantFixture(t)
			b.Revision++
			b.TV.AccessToken, b.Agent.AccessToken = "new-tv", "new-agent"
			b.TV.Generation++
			b.Agent.Generation++
			var mutate func(*nodeprotocol.CredentialGrant, *nodeprotocol.StorageCredential)
			switch mode {
			case "same-revision":
				b.Revision--
			case "extra-action":
				mutate = func(n *nodeprotocol.CredentialGrant, _ *nodeprotocol.StorageCredential) {
					n.AllowedActions = append(n.AllowedActions, nodeprotocol.StorageSourceGrantRead)
				}
			case "missing-action":
				mutate = func(n *nodeprotocol.CredentialGrant, _ *nodeprotocol.StorageCredential) {
					n.AllowedActions = []string{nodeprotocol.StorageSourceGrantRead}
				}
			case "client-device":
				b.Agent.ClientDeviceID = "other-device"
			case "generation":
				b.TV.Generation = 0
			case "proof":
				b.Proofs[0].Generation++
			case "revision-envelope":
				mutate = func(n *nodeprotocol.CredentialGrant, _ *nodeprotocol.StorageCredential) { n.CredentialRevision++ }
			case "revision-rollback":
				b.Revision = 1
			case "refresh":
				b.Agent.RefreshToken = "not-allowed"
			case "web":
				b.Web = &quark.WebAuthorization{Cookie: "not-allowed"}
			case "root":
				mutate = func(_ *nodeprotocol.CredentialGrant, c *nodeprotocol.StorageCredential) { c.RootID = "other-root" }
			}
			saveNextQuarkGrant(t, g, b, mutate)
			if mode == "cancelled" {
				if _, err := a.store.Cancel(context.Background(), g.serverID, g.grant.OperationKey, a.now()); err != nil {
					t.Fatal(err)
				}
			}
			raw, err := g.load(context.Background())
			if mode != "renew" {
				if err == nil || len(raw) != 0 || g.grant.GrantID != "grant-old" {
					t.Fatal("invalid continuation adopted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := quark.ParseCredentials(raw)
			if err != nil || got.TV.AccessToken != "new-tv" || got.Agent.AccessToken != "new-agent" || g.grant.GrantID != "grant-new" {
				t.Fatal("new access credentials not installed")
			}
		})
	}
}

type quarkContextBlockedBody struct {
	ctx     context.Context
	entered chan struct{}
}

func (r quarkContextBlockedBody) Read([]byte) (int, error) {
	close(r.entered)
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}
func (r quarkContextBlockedBody) Close() error { return nil }

func TestQuarkGuardCancelsBlockedMediaWhenGrantOrLeaseEnds(t *testing.T) {
	for _, mode := range []string{"grant-expired", "lease-expired", "operation-cancelled", "context-cancelled"} {
		t.Run(mode, func(t *testing.T) {
			a, g, _ := quarkGrantFixture(t)
			base := time.Now().UTC()
			if mode == "lease-expired" {
				g.grant.ExpiresAt = base.Add(4 * time.Hour)
			}
			var offset atomic.Int64
			a.now = func() time.Time { return base.Add(time.Duration(offset.Load())) }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			guarded, stop, err := quarkGuardedContext(ctx, g.check)
			if err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{})
			body := &quarkGuardedBody{ReadCloser: quarkContextBlockedBody{guarded, entered}, ctx: guarded, guard: g.check, stop: stop}
			defer body.Close()
			done := make(chan error, 1)
			go func() { _, err := io.ReadAll(body); done <- err }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("media read did not enter blocking transport")
			}
			switch mode {
			case "grant-expired":
				offset.Store(int64(2 * time.Minute))
			case "lease-expired":
				offset.Store(int64(2 * time.Hour))
			case "operation-cancelled":
				if _, err = a.store.Cancel(ctx, g.serverID, g.grant.OperationKey, a.now()); err != nil {
					t.Fatal(err)
				}
			case "context-cancelled":
				cancel()
			}
			select {
			case err = <-done:
				if err == nil {
					t.Fatal("cancelled media completed")
				}
			case <-time.After(4 * time.Second):
				t.Fatal("blocked media ignored authorization/context cancellation")
			}
		})
	}
}
