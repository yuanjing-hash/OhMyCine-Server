//go:build quark_live

package handlers_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

const observationPurpose = "quark-native-acceptance:observation-journal:v1"
const observationFixtureSHA = "6ebd9b2c19d874d05b88127044186c8c441bdab9e9e62cd17396a9d4d6307a9c"
const observationFixtureSize = 10998

func observationError() error { return errors.New("observation_guard_or_private_state_rejected") }

type observationLocalFailure string

func (e observationLocalFailure) Error() string { return string(e) }

type observationAgentSession struct {
	Version    int       `json:"version"`
	App        string    `json:"app_fingerprint"`
	Device     string    `json:"device_id"`
	Client     string    `json:"client_device_id"`
	User       string    `json:"user_id"`
	Access     string    `json:"access_token"`
	Generation uint64    `json:"generation"`
	FirstParty bool      `json:"first_party_authorization"`
	Updated    time.Time `json:"updated_at"`
}
type observationOwned struct {
	ID     string `json:"fid"`
	Name   string `json:"name"`
	Parent string `json:"parent_fid"`
}
type observationLedger struct {
	Version       int                         `json:"version"`
	App           string                      `json:"app_fingerprint"`
	User          string                      `json:"user_id"`
	Device        string                      `json:"device_id"`
	Client        string                      `json:"client_device_id"`
	Generation    uint64                      `json:"generation"`
	SessionAt     time.Time                   `json:"session_updated_at"`
	Nonce         string                      `json:"nonce"`
	RootName      string                      `json:"root_name"`
	Owned         map[string]observationOwned `json:"owned"`
	Pending       json.RawMessage             `json:"pending"`
	UploadPending json.RawMessage             `json:"upload_pending"`
}
type observationAgentState struct {
	lock    *database.ExclusiveRuntime
	session observationAgentSession
	ledger  observationLedger
	files   map[string][]byte
}

func openObservationAgent(dir string) (*observationAgentState, error) {
	if !filepath.IsAbs(dir) {
		return nil, observationLocalFailure("agent_path_invalid")
	}
	// Validate before opening the lock; never creates a login directory/key.
	key, err := liveTVRead(filepath.Join(dir, "credentials.key"), 1024, false)
	if err != nil {
		return nil, observationLocalFailure("agent_key_unreadable")
	}
	lock, err := database.AcquireExclusiveRuntime(filepath.Join(dir, "session.enc.json"))
	if err != nil {
		return nil, observationLocalFailure("agent_state_locked")
	}
	s := &observationAgentState{lock: lock, files: map[string][]byte{filepath.Join(dir, "credentials.key"): key}}
	ok := false
	defer func() {
		if !ok {
			lock.Close()
		}
	}()
	cipher, err := credential.Open("", string(key))
	if err != nil {
		return nil, observationLocalFailure("agent_key_invalid")
	}
	load := func(name, purpose string, out any) error {
		path := filepath.Join(dir, name)
		raw, e := liveTVRead(path, 1<<20, false)
		if e != nil {
			return observationError()
		}
		var envelope struct {
			Version    int
			Ciphertext string
		}
		if liveTVDecode(raw, &envelope) != nil || envelope.Version != 1 {
			return observationError()
		}
		plain, e := cipher.Decrypt(purpose, envelope.Ciphertext)
		// Legacy state includes fields not needed here (including refresh). Decode
		// only this view; never retain that token in our journal or child Server.
		if e != nil || len(plain) > 1<<20 || json.Unmarshal([]byte(plain), out) != nil {
			return observationError()
		}
		s.files[path] = raw
		return nil
	}
	if load("session.enc.json", "quark-original-302-probe:state:v1", &s.session) != nil {
		return nil, observationLocalFailure("agent_session_unreadable")
	}
	if load("capability-ledger.enc.json", "quark-capability-validation:ownership-intent:v1", &s.ledger) != nil {
		return nil, observationLocalFailure("agent_ownership_unreadable")
	}
	a, l := s.session, s.ledger
	nonce, e := hex.DecodeString(l.Nonce)
	if a.Version != 1 || !a.FirstParty || !liveTVScalar(a.Device, 4096) || !liveTVScalar(a.Client, 256) || !liveTVScalar(a.User, 4096) || !liveTVScalar(a.Access, 32768) || a.Generation == 0 || a.Updated.IsZero() || a.Updated.After(time.Now().Add(time.Minute)) {
		return nil, observationLocalFailure("agent_session_invalid")
	}
	if a.App != quark.ObservationAgentProfileFingerprint() {
		return nil, observationLocalFailure("agent_profile_mismatch")
	}
	if l.Version != 1 || e != nil || len(nonce) != 16 || l.RootName != "OMC-Quark-Validation-20261021-"+l.Nonce || l.Owned["root"].Name != l.RootName || l.Owned["root"].Parent != "0" || !liveTVScalar(l.Owned["root"].ID, 4096) {
		return nil, observationLocalFailure("agent_ownership_invalid")
	}
	if l.App != a.App || l.User != a.User || l.Device != a.Device || l.Client != a.Client || l.Generation != a.Generation || !l.SessionAt.Equal(a.Updated) {
		return nil, observationLocalFailure("agent_ownership_session_mismatch")
	}
	for _, pending := range []json.RawMessage{l.Pending, l.UploadPending} {
		if len(pending) > 0 && string(pending) != "null" {
			return nil, observationLocalFailure("agent_unfinished_operation")
		}
	}
	if s.unchanged() != nil {
		return nil, observationLocalFailure("agent_state_changed")
	}
	ok = true
	return s, nil
}
func (s *observationAgentState) unchanged() error {
	for p, b := range s.files {
		current, e := liveTVRead(p, 1<<20, false)
		if e != nil || !bytes.Equal(b, current) {
			return observationError()
		}
	}
	return nil
}

type observationState struct {
	Version                                                                                                                    int
	Nonce, RootName, RootACK, AgentProfile, AgentDevice, AgentClient, AgentAccount, TVProfile, TVDevice, TVAccount, FixtureSHA string
	AgentGeneration                                                                                                            uint64
	AgentUpdated                                                                                                               time.Time
	ProofID, DirectoryID, UploadID, DirectoryName, UploadName                                                                  string
	Bundle                                                                                                                     quark.Credentials
	Operations                                                                                                                 map[string]cloud.OperationReceipt
	Requests, API, ReadBytes, Reads, PreWrites, PutWrites, FinishWrites, DirectoryWrites, BrokerWrites                         int
	Writes                                                                                                                     map[string]bool
	Started, LastDispatch                                                                                                      time.Time
	RootVerified, Completed                                                                                                    bool
}
type observationStore struct {
	mu       sync.Mutex
	dir      string
	cipher   *credential.Store
	lock     *database.ExclusiveRuntime
	state    observationState
	poisoned bool
}

func (s *observationStore) save() error {
	if s.poisoned {
		return observationError()
	}
	data, err := json.Marshal(s.state)
	if err != nil {
		s.poisoned = true
		return observationError()
	}
	encrypted, err := s.cipher.Encrypt(observationPurpose, string(data))
	if err != nil {
		s.poisoned = true
		return observationError()
	}
	f, err := os.CreateTemp(s.dir, ".observation-*.tmp")
	if err != nil {
		s.poisoned = true
		return observationError()
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = liveTVCopySecurity(filepath.Join(s.dir, "observation.key"), f.Name()); err == nil {
		err = f.Chmod(0600)
	}
	if err == nil {
		_, err = f.WriteString(encrypted)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(s.dir, "observation.enc"))
	}
	if err != nil {
		s.poisoned = true
		return observationError()
	}
	return nil
}
func (s *observationStore) Load(_ context.Context, id string) (cloud.OperationReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned {
		return cloud.OperationReceipt{}, observationError()
	}
	r, ok := s.state.Operations[id]
	if !ok {
		return r, cloud.ErrOperationNotFound
	}
	r.Payload = append(json.RawMessage(nil), r.Payload...)
	return r, nil
}
func (s *observationStore) allowedReceipt(r cloud.OperationReceipt) bool {
	if r.Provider != cloud.ProviderQuark || r.Route != string(quark.RouteAgent) || r.AccountID != s.state.AgentAccount {
		return false
	}
	return r.ID == s.state.ProofID && r.Kind == "space_proof" || (r.ID == s.state.ProofID+"/upload" || r.ID == s.state.UploadID) && r.Kind == "upload" || r.ID == s.state.DirectoryID && r.Kind == "mkdir"
}
func (s *observationStore) Create(_ context.Context, r cloud.OperationReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.state.Operations[r.ID]; exists || r.Revision != 1 || !s.allowedReceipt(r) {
		return observationError()
	}
	r.Payload = append(json.RawMessage(nil), r.Payload...)
	s.state.Operations[r.ID] = r
	return s.save()
}
func (s *observationStore) Update(_ context.Context, expected uint64, r cloud.OperationReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.state.Operations[r.ID]
	if !ok || old.Revision != expected || r.Revision != expected+1 || old.Kind != r.Kind || !s.allowedReceipt(r) {
		return observationError()
	}
	r.Payload = append(json.RawMessage(nil), r.Payload...)
	s.state.Operations[r.ID] = r
	return s.save()
}
func (s *observationStore) credentials(context.Context) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned {
		return nil, observationError()
	}
	return json.Marshal(s.state.Bundle)
}
func (s *observationStore) persist(_ context.Context, expected uint64, raw json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, e := quark.ParseCredentials(raw)
	old := s.state.Bundle
	if e != nil || c.TV == nil || c.Agent == nil || c.Web != nil || c.TV.RefreshToken != "" || c.Agent.RefreshToken != "" || old.Revision != expected || c.Revision != expected+1 || *c.TV != *old.TV || *c.Agent != *old.Agent {
		return observationError()
	}
	for _, p := range c.Proofs {
		if p.ReceiptID != s.state.ProofID || p.TVAccount != s.state.TVAccount || p.AgentAccount != s.state.AgentAccount || p.Path != "/"+s.state.RootName || s.state.Operations[p.ReceiptID].State != "confirmed" {
			return observationError()
		}
	}
	s.state.Bundle = c
	return s.save()
}
func (s *observationStore) count() int { s.mu.Lock(); defer s.mu.Unlock(); return s.state.Requests }

func openObservationStore(dir string, a *observationAgentState, tv *liveTVStore) (*observationStore, error) {
	if !filepath.IsAbs(dir) || strings.EqualFold(filepath.Clean(dir), filepath.Clean(tv.dir)) {
		return nil, observationError()
	}
	if err := observationCheckPrivateDirectory(dir); err != nil {
		return nil, err
	}
	lock, err := database.AcquireExclusiveRuntime(filepath.Join(dir, "observation"))
	if err != nil {
		return nil, observationError()
	}
	ok := false
	defer func() {
		if !ok {
			lock.Close()
		}
	}()
	s := &observationStore{dir: dir, lock: lock}
	journal, err := liveTVRead(filepath.Join(dir, "observation.enc"), 2<<20, true)
	if err != nil {
		return nil, observationError()
	}
	key, err := liveTVRead(filepath.Join(dir, "observation.key"), 1024, true)
	if err != nil {
		return nil, observationError()
	}
	if len(journal) > 0 && len(key) == 0 {
		return nil, observationError()
	}
	if len(journal) == 0 {
		entries, e := os.ReadDir(dir)
		if e != nil {
			return nil, observationError()
		}
		for _, entry := range entries {
			if entry.Name() != "observation.runtime.lock" {
				return nil, observationError()
			}
		}
	}
	if len(key) == 0 {
		s.cipher, err = credential.Open(filepath.Join(dir, "observation.key"), "")
	} else {
		s.cipher, err = credential.Open("", string(key))
	}
	if err != nil {
		return nil, observationError()
	}
	if len(journal) > 0 {
		plain, e := s.cipher.Decrypt(observationPurpose, string(journal))
		if e != nil || liveTVDecode([]byte(plain), &s.state) != nil {
			return nil, observationError()
		}
	} else {
		var nonce [16]byte
		if _, err = rand.Read(nonce[:]); err != nil {
			return nil, observationError()
		}
		n := hex.EncodeToString(nonce[:])
		l, v := a.ledger, a.session
		s.state = observationState{Version: 1, Nonce: n, RootName: l.RootName, RootACK: l.Owned["root"].ID, AgentProfile: v.App, AgentDevice: v.Device, AgentClient: v.Client, AgentAccount: v.User, AgentGeneration: v.Generation, AgentUpdated: v.Updated, TVProfile: tv.session.ProfileFingerprint, TVDevice: tv.session.DeviceID, TVAccount: tv.session.UserID, FixtureSHA: observationFixtureSHA, ProofID: "observation-proof-" + n, DirectoryID: "observation-directory-" + n, UploadID: "observation-video-" + n, DirectoryName: "native-observation-" + n, UploadName: "omc-synthetic-" + n + ".mp4", Operations: map[string]cloud.OperationReceipt{}, Writes: map[string]bool{}, Started: time.Now().UTC()}
		if s.save() != nil {
			return nil, observationError()
		}
	}
	v, l, st := a.session, a.ledger, s.state
	if !observationManifestValid(st) {
		return nil, observationError()
	}
	if st.Version != 1 || len(st.Nonce) != 32 || st.RootName != l.RootName || st.RootACK != l.Owned["root"].ID || st.AgentProfile != v.App || st.AgentDevice != v.Device || st.AgentClient != v.Client || st.AgentAccount != v.User || st.AgentGeneration != v.Generation || !st.AgentUpdated.Equal(v.Updated) || st.TVProfile != tv.session.ProfileFingerprint || st.TVDevice != tv.session.DeviceID || st.TVAccount != tv.session.UserID || st.FixtureSHA != observationFixtureSHA || st.Operations == nil || st.Writes == nil || st.Started.IsZero() || st.Started.After(time.Now()) || !st.Completed && (time.Since(st.Started) > 12*time.Minute || st.Requests >= 180 || st.API >= 160) {
		return nil, observationError()
	}
	ok = true
	return s, nil
}

func observationManifestValid(st observationState) bool {
	nonce, e := hex.DecodeString(st.Nonce)
	if e != nil || len(nonce) != 16 || st.ProofID != "observation-proof-"+st.Nonce || st.DirectoryID != "observation-directory-"+st.Nonce || st.UploadID != "observation-video-"+st.Nonce || st.DirectoryName != "native-observation-"+st.Nonce || st.UploadName != "omc-synthetic-"+st.Nonce+".mp4" {
		return false
	}
	return st.Requests >= 0 && st.Requests <= 180 && st.API >= 0 && st.API <= 160 && st.API <= st.Requests && st.ReadBytes >= 0 && st.ReadBytes <= 64<<10 && st.Reads >= 0 && st.Reads <= 8 && st.PreWrites >= 0 && st.PreWrites <= 2 && st.PutWrites >= 0 && st.PutWrites <= 2 && st.FinishWrites >= 0 && st.FinishWrites <= 2 && st.DirectoryWrites >= 0 && st.DirectoryWrites <= 1 && st.BrokerWrites >= 0 && st.BrokerWrites <= 1
}

func observationFixture(path string) ([]byte, error) {
	b, e := liveTVRead(path, observationFixtureSize, false)
	if e != nil || len(b) != observationFixtureSize {
		return nil, observationError()
	}
	h := sha256.Sum256(b)
	if hex.EncodeToString(h[:]) != observationFixtureSHA {
		return nil, observationError()
	}
	return b, nil
}
