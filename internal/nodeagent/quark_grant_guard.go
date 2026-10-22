package nodeagent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

// One synchronized holder supplies both the native client and continued file
// I/O. Renewed access credentials must be loaded before signing a request.
type quarkGrantState struct {
	mu                   sync.Mutex
	agent                *Agent
	serverID, planDigest string
	grant                nodeprotocol.CredentialGrant
	credential           nodeprotocol.StorageCredential
}

func (a *Agent) newQuarkGrantState(serverID, planDigest string, grant nodeprotocol.CredentialGrant, credential nodeprotocol.StorageCredential) *quarkGrantState {
	credential.Quark = append(json.RawMessage(nil), credential.Quark...)
	grant.AllowedActions = append([]string(nil), grant.AllowedActions...)
	grant.Credential = append(json.RawMessage(nil), grant.Credential...)
	return &quarkGrantState{agent: a, serverID: serverID, planDigest: planDigest, grant: grant, credential: credential}
}
func (a *Agent) quarkGrantGuard(serverID, planDigest string, grant nodeprotocol.CredentialGrant, credential nodeprotocol.StorageCredential) func(context.Context) error {
	return a.newQuarkGrantState(serverID, planDigest, grant, credential).check
}
func (g *quarkGrantState) check(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.checkLocked(ctx, true)
}

// BeforeCall only validates: adopting after construction would send old tokens.
func (g *quarkGrantState) validateCurrent(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.checkLocked(ctx, false)
}
func (g *quarkGrantState) load(ctx context.Context) (json.RawMessage, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.checkLocked(ctx, true); err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), g.credential.Quark...), nil
}
func (g *quarkGrantState) checkLocked(ctx context.Context, renew bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now := g.agent.now()
	if _, err := g.agent.store.ActiveOperationPlan(ctx, g.serverID, g.grant.OperationKey, g.planDigest, now); err != nil {
		return err
	}
	if renew && g.grant.ExpiresAt.Before(now.Add(2*time.Minute)) {
		var raw string
		err := g.agent.store.db.QueryRowContext(ctx, `SELECT envelope_json FROM credential_grants WHERE paired_server_id=? AND operation_key=? AND task_id=? AND resource_kind=? AND resource_id=? AND expires_at>? ORDER BY expires_at DESC LIMIT 1`, g.serverID, g.grant.OperationKey, g.grant.TaskID, g.grant.ResourceKind, g.grant.ResourceID, g.grant.ExpiresAt).Scan(&raw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			var envelope nodeprotocol.CredentialGrantEnvelope
			if json.Unmarshal([]byte(raw), &envelope) != nil {
				return errors.New(nodeprotocol.ErrorCredentialExpired)
			}
			next, err := nodeprotocol.OpenCredentialGrant(g.agent.sealingPrivateKey, envelope, now)
			if err != nil {
				return err
			}
			if err = g.adopt(next); err != nil {
				return err
			}
		}
	}
	if !g.grant.ExpiresAt.After(now) {
		return errors.New(nodeprotocol.ErrorCredentialExpired)
	}
	return ctx.Err()
}
func (g *quarkGrantState) adopt(next nodeprotocol.CredentialGrant) error {
	old := g.grant
	if next.NodeID != old.NodeID || next.TaskID != old.TaskID || next.OperationKey != old.OperationKey || next.ResourceKind != old.ResourceKind || next.ResourceID != old.ResourceID || next.CredentialRevision < old.CredentialRevision {
		return ErrPlanConflict
	}
	actions, nextActions := slices.Clone(old.AllowedActions), slices.Clone(next.AllowedActions)
	slices.Sort(actions)
	slices.Sort(nextActions)
	if !slices.Equal(actions, nextActions) {
		return ErrPlanConflict
	}
	var payload, previous nodeprotocol.StorageSourceCredential
	if json.Unmarshal(next.Credential, &payload) != nil {
		return ErrPlanConflict
	}
	if len(old.Credential) != 0 && json.Unmarshal(old.Credential, &previous) != nil {
		return ErrPlanConflict
	}
	if payload.SourceURI != previous.SourceURI {
		return ErrPlanConflict
	}
	newCredential := nodeprotocol.StorageCredential{ProviderType: payload.ProviderType, Cookie: payload.Cookie, RootID: payload.RootID, Quark: payload.Quark}
	before, err := validateQuarkGrant(g.credential)
	if err != nil {
		return err
	}
	after, err := validateQuarkGrant(newCredential)
	if err != nil {
		return err
	}
	if newCredential.RootID != g.credential.RootID || before.Revision != old.CredentialRevision || after.Revision != next.CredentialRevision || after.Revision < before.Revision || !sameQuarkGrantAuthorization(before.TV, after.TV) || !sameQuarkGrantAuthorization(before.Agent, after.Agent) || !reflect.DeepEqual(before.Proofs, after.Proofs) {
		return ErrPlanConflict
	}
	if after.Revision == before.Revision && !reflect.DeepEqual(before, after) {
		return ErrPlanConflict
	}
	g.grant, g.credential = next, newCredential
	return nil
}
func sameQuarkGrantAuthorization(before, after *quark.Authorization) bool {
	return before != nil && after != nil && before.AccountID == after.AccountID && before.DeviceID == after.DeviceID && before.ClientDeviceID == after.ClientDeviceID && before.IdentityVerified && after.IdentityVerified && after.Generation >= before.Generation
}
