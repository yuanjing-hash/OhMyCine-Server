//go:build quark_live

package handlers_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

const agentRecoveryName = "native-agent-continuation.enc.json"
const agentRecoveryPurpose = "quark-native-acceptance:agent-continuation:v1"

type agentRecoveryState struct {
	Version                       int `json:"version"`
	SessionSHA, LedgerSHA, KeySHA string
	Current                       quark.Credentials
	AttemptRevision               uint64
	AttemptPairSHA                string
	AttemptDenied                 bool
}

type agentRecoveryStore struct {
	original *observationAgentState
	dir      string
	cipher   *credential.Store
	state    agentRecoveryState
}

// Caller retains original.lock for the full lifetime. Refresh replacements are
// saved separately: old generation/time-bound ownership evidence is immutable.
func openAgentRecoveryStore(dir string, original *observationAgentState) (*agentRecoveryStore, error) {
	if original == nil || original.lock == nil || original.unchanged() != nil {
		return nil, observationError()
	}
	key := original.files[filepath.Join(dir, "credentials.key")]
	c, err := credential.Open("", string(key))
	if err != nil {
		return nil, observationError()
	}
	s := &agentRecoveryStore{original: original, dir: dir, cipher: c}
	s.state = agentRecoveryState{Version: 1, SessionSHA: followDigest(original.files[filepath.Join(dir, "session.enc.json")]), LedgerSHA: followDigest(original.files[filepath.Join(dir, "capability-ledger.enc.json")]), KeySHA: followDigest(key)}
	raw, err := liveTVRead(filepath.Join(dir, agentRecoveryName), 1<<20, true)
	if err != nil {
		return nil, observationError()
	}
	if len(raw) == 0 {
		var envelope struct {
			Version    int
			Ciphertext string
		}
		if liveTVDecode(original.files[filepath.Join(dir, "session.enc.json")], &envelope) != nil || envelope.Version != 1 {
			return nil, observationError()
		}
		plain, e := c.Decrypt("quark-original-302-probe:state:v1", envelope.Ciphertext)
		var full struct {
			observationAgentSession
			Refresh string `json:"refresh_token"`
		}
		if e != nil || len(plain) > 1<<20 || json.Unmarshal([]byte(plain), &full) != nil || !reflect.DeepEqual(full.observationAgentSession, original.session) || !liveTVScalar(full.Refresh, 32768) {
			return nil, liveTVError("agent_refresh_original_pair_unavailable")
		}
		a := original.session
		s.state.Current = quark.NewCredentials()
		s.state.Current.Revision = 1
		s.state.Current.Agent = &quark.Authorization{AccessToken: a.Access, RefreshToken: full.Refresh, DeviceID: a.Device, ClientDeviceID: a.Client, AccountID: a.User, Generation: a.Generation, IdentityVerified: true}
	} else {
		var envelope struct {
			Version    int
			Ciphertext string
		}
		if liveTVDecode(raw, &envelope) != nil || envelope.Version != 1 {
			return nil, observationError()
		}
		plain, e := c.Decrypt(agentRecoveryPurpose, envelope.Ciphertext)
		var st agentRecoveryState
		if e != nil || len(plain) > 1<<20 || liveTVDecode([]byte(plain), &st) != nil || st.SessionSHA != s.state.SessionSHA || st.LedgerSHA != s.state.LedgerSHA || st.KeySHA != s.state.KeySHA {
			return nil, liveTVError("agent_refresh_original_evidence_changed")
		}
		s.state = st
	}
	if s.validate() != nil {
		return nil, observationError()
	}
	return s, nil
}

func agentRecoveryPairSHA(c quark.Credentials) string {
	raw, _ := json.Marshal(c)
	return followDigest(raw)
}

func (s *agentRecoveryStore) validate() error {
	st := s.state
	c := st.Current
	a := c.Agent
	o := s.original.session
	if st.Version != 1 || c.Version != 1 || c.Revision == 0 || c.TV != nil || c.Web != nil || len(c.Proofs) != 0 || a == nil || a.AccountID != o.User || a.DeviceID != o.Device || a.ClientDeviceID != o.Client || a.Generation < o.Generation || !liveTVScalar(a.AccessToken, 32768) || !liveTVScalar(a.RefreshToken, 32768) {
		return observationError()
	}
	if st.AttemptRevision == 0 {
		if st.AttemptPairSHA != "" || st.AttemptDenied || !a.IdentityVerified {
			return observationError()
		}
	} else {
		if len(st.AttemptPairSHA) != 64 || st.AttemptRevision > c.Revision || st.AttemptRevision+1 < c.Revision {
			return observationError()
		}
		if a.IdentityVerified && (st.AttemptRevision != c.Revision || st.AttemptPairSHA != agentRecoveryPairSHA(c)) {
			return observationError()
		}
		if !a.IdentityVerified && (st.AttemptRevision+1 != c.Revision || st.AttemptDenied) {
			return observationError()
		}
	}
	return nil
}

func (s *agentRecoveryStore) save() error {
	if s.validate() != nil || s.original.unchanged() != nil {
		return observationError()
	}
	raw, err := json.Marshal(s.state)
	if err != nil {
		return observationError()
	}
	encrypted, err := s.cipher.Encrypt(agentRecoveryPurpose, string(raw))
	if err != nil {
		return observationError()
	}
	raw, err = json.Marshal(struct {
		Version    int
		Ciphertext string
	}{1, encrypted})
	if err != nil {
		return observationError()
	}
	target := filepath.Join(s.dir, agentRecoveryName)
	if _, err = liveTVRead(target, 1<<20, true); err != nil {
		return observationError()
	}
	f, err := os.CreateTemp(s.dir, ".agent-continuation-*.tmp")
	if err != nil {
		return observationError()
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = liveTVCopySecurity(filepath.Join(s.dir, "session.enc.json"), f.Name()); err != nil {
		return observationError()
	}
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil || s.original.unchanged() != nil {
		return observationError()
	}
	if os.Rename(f.Name(), target) != nil {
		return observationError()
	}
	return nil
}

func (s *agentRecoveryStore) persist(ctx context.Context, revision uint64, raw json.RawMessage) error {
	if ctx.Err() != nil || s.state.Current.Revision != revision || s.state.AttemptRevision == 0 {
		return observationError()
	}
	next, err := quark.ParseCredentials(raw)
	if err != nil || next.Revision != revision+1 || next.TV != nil || next.Web != nil || len(next.Proofs) != 0 || next.Agent == nil {
		return observationError()
	}
	old := s.state.Current.Agent
	n := next.Agent
	if n.AccountID != old.AccountID || n.DeviceID != old.DeviceID || n.ClientDeviceID != old.ClientDeviceID {
		return liveTVError("agent_refresh_identity_mismatch")
	}
	before := s.state
	if !n.IdentityVerified {
		if !old.IdentityVerified || n.Generation != old.Generation+1 || s.state.AttemptRevision != revision {
			return observationError()
		}
	} else {
		prior := *old
		prior.IdentityVerified = true
		if old.IdentityVerified || !reflect.DeepEqual(prior, *n) {
			return observationError()
		}
		s.state.AttemptRevision = 0
		s.state.AttemptPairSHA = ""
		s.state.AttemptDenied = false
	}
	s.state.Current = next
	if err = s.save(); err != nil {
		s.state = before
		return err
	}
	return nil
}

func (s *agentRecoveryStore) ready(ctx context.Context, auth liveTVAuthenticator) (bool, error) {
	if s.original.unchanged() != nil {
		return false, observationError()
	}
	if !s.state.Current.Agent.IdentityVerified {
		_, err := auth.Confirm(ctx, quark.RouteAgent, s.state.Current, s.persist)
		return err == nil, err
	}
	err := auth.Verify(ctx, quark.RouteAgent, s.state.Current)
	if err == nil {
		return false, nil
	}
	if largefileRefreshCode(err) != quark.CodeAuth {
		return false, err
	}
	if s.state.AttemptRevision != 0 {
		if s.state.AttemptDenied {
			return false, liveTVError("agent_refresh_denied")
		}
		return false, liveTVError("agent_refresh_uncertain")
	}
	s.state.AttemptRevision = s.state.Current.Revision
	s.state.AttemptPairSHA = agentRecoveryPairSHA(s.state.Current)
	if s.save() != nil {
		return false, observationError()
	}
	_, err = auth.Refresh(ctx, quark.RouteAgent, s.state.Current, s.persist)
	if err != nil && s.state.Current.Agent.IdentityVerified && largefileRefreshCode(err) == quark.CodeAuth {
		s.state.AttemptDenied = true
		_ = s.save()
	}
	return err == nil, err
}

// Access snapshot consumers do not retain refresh material. The original
// generation-bound ledger/donor identity is still read unchanged separately.
func agentRecoveryAccessSnapshot(dir string, original *observationAgentState) (*quark.Authorization, error) {
	if original == nil || original.lock == nil || original.unchanged() != nil {
		return nil, observationError()
	}
	raw, err := liveTVRead(filepath.Join(dir, agentRecoveryName), 1<<20, true)
	if err != nil {
		return nil, observationError()
	}
	if len(raw) == 0 {
		a := original.session
		return &quark.Authorization{AccessToken: a.Access, AccountID: a.User, DeviceID: a.Device, ClientDeviceID: a.Client, Generation: a.Generation, IdentityVerified: true}, nil
	}
	s, err := openAgentRecoveryStore(dir, original)
	if err != nil {
		return nil, err
	}
	if s.state.AttemptRevision != 0 || !s.state.Current.Agent.IdentityVerified {
		return nil, liveTVError("agent_refresh_requires_explicit_recovery")
	}
	a := *s.state.Current.Agent
	a.RefreshToken = ""
	return &a, nil
}
