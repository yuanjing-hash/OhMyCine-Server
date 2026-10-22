//go:build quark_live

package handlers_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

const proofCleanupPurpose = "quark-native-acceptance:proof-cleanup:v1"
const proofCleanupDuration = 10 * time.Minute
const proofCleanupMetadataLimit = 256 << 10
const proofCleanupMetadataBudget = 8 << 20

// The donor is a completed, independently locked observation journal. Only
// its root proof is needed: no service database, fixture or media file is read.
type proofCleanupIdentity struct {
	JournalSHA, KeySHA, RootName, RootACK string
	Proof                                 quark.SpaceProof
}

func proofCleanupDonor(st observationState, journal, key []byte) (proofCleanupIdentity, error) {
	var out proofCleanupIdentity
	if !st.Completed || !st.RootVerified || !observationManifestValid(st) || len(journal) == 0 || len(key) == 0 || !strings.HasPrefix(st.RootName, "OMC-Quark-Validation-20261021-") || st.Bundle.TV == nil || st.Bundle.Agent == nil || st.Bundle.Web != nil {
		return out, observationError()
	}
	r := st.Operations[st.ProofID]
	var p struct{ Proof *quark.SpaceProof }
	if r.Kind != "space_proof" || r.State != "confirmed" || r.Provider != cloud.ProviderQuark || r.AccountID != st.AgentAccount || json.Unmarshal(r.Payload, &p) != nil || p.Proof == nil {
		return out, observationError()
	}
	v := *p.Proof
	if v.ReceiptID != st.ProofID || v.Path != "/"+st.RootName || v.AgentAccount != st.AgentAccount || v.TVAccount != st.TVAccount || !st.Bundle.HasProof(v.TVRoot, v.AgentRoot) {
		return out, observationError()
	}
	if _, ok := followRawID(v.AgentRoot, quark.RouteAgent, v.AgentAccount); !ok {
		return out, observationError()
	}
	if _, ok := followRawID(v.TVRoot, quark.RouteTV, v.TVAccount); !ok {
		return out, observationError()
	}
	return proofCleanupIdentity{followDigest(journal), followDigest(key), st.RootName, st.RootACK, v}, nil
}

type proofCleanupState struct {
	Version                                 int
	Identity                                proofCleanupIdentity
	Nonce, ProofID                          string
	Bundle                                  quark.Credentials
	Operations                              map[string]cloud.OperationReceipt
	Writes                                  map[string]bool
	Requests, API, ReadBytes, MetadataBytes int
	Started, Deadline, LastDispatch         time.Time
	RootVerified, Completed                 bool
}

type proofCleanupStore struct {
	mu       sync.Mutex
	dir      string
	cipher   *credential.Store
	lock     *database.ExclusiveRuntime
	state    proofCleanupState
	poisoned bool
}

func proofCleanupManifestValid(st proofCleanupState) bool {
	n, err := hex.DecodeString(st.Nonce)
	c, i := st.Bundle, st.Identity
	if err != nil || len(n) != 16 || st.Version != 1 || st.ProofID != "proof-cleanup-"+st.Nonce || st.Started.IsZero() || st.Started.After(time.Now()) || !st.Deadline.Equal(st.Started.Add(proofCleanupDuration)) || st.Operations == nil || st.Writes == nil || len(st.Operations) > 2 || len(st.Writes) > 4 || c.TV == nil || c.Agent == nil || c.Web != nil || c.TV.RefreshToken != "" || c.Agent.RefreshToken != "" || !c.TV.IdentityVerified || !c.Agent.IdentityVerified || c.Agent.AccountID != i.Proof.AgentAccount || c.TV.AccountID != i.Proof.TVAccount {
		return false
	}
	for key, reserved := range st.Writes {
		if !reserved || key != "pre" && key != "put" && key != "finish" && key != "delete" {
			return false
		}
	}
	return st.Requests >= 0 && st.Requests <= 128 && st.API >= 0 && st.API <= 96 && st.API <= st.Requests && st.ReadBytes >= 0 && st.ReadBytes <= 128 && st.MetadataBytes >= 0 && st.MetadataBytes <= proofCleanupMetadataBudget
}

func (s *proofCleanupStore) save() error {
	if s.poisoned || !proofCleanupManifestValid(s.state) {
		return observationError()
	}
	data, err := json.Marshal(s.state)
	if err != nil {
		return observationError()
	}
	sealed, err := s.cipher.Encrypt(proofCleanupPurpose, string(data))
	if err != nil {
		return observationError()
	}
	f, err := os.CreateTemp(s.dir, ".proof-cleanup-*.tmp")
	if err != nil {
		return observationError()
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = liveTVCopySecurity(filepath.Join(s.dir, "proof-cleanup.key"), f.Name()); err == nil {
		err = f.Chmod(0600)
	}
	if err == nil {
		_, err = f.WriteString(sealed)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(s.dir, "proof-cleanup.enc"))
	}
	if err != nil {
		s.poisoned = true
		return observationError()
	}
	return nil
}

func openProofCleanupStore(dir string, identity proofCleanupIdentity, access quark.Credentials) (*proofCleanupStore, error) {
	if !filepath.IsAbs(dir) || observationCheckPrivateDirectory(dir) != nil || access.TV == nil || access.Agent == nil || access.Web != nil || access.TV.RefreshToken != "" || access.Agent.RefreshToken != "" {
		return nil, observationError()
	}
	lock, err := database.AcquireExclusiveRuntime(filepath.Join(dir, "proof-cleanup"))
	if err != nil {
		return nil, observationError()
	}
	ok := false
	defer func() {
		if !ok {
			lock.Close()
		}
	}()
	s := &proofCleanupStore{dir: dir, lock: lock}
	journal, err := liveTVRead(filepath.Join(dir, "proof-cleanup.enc"), 2<<20, true)
	if err != nil {
		return nil, observationError()
	}
	key, err := liveTVRead(filepath.Join(dir, "proof-cleanup.key"), 1024, true)
	if err != nil || len(journal) > 0 && len(key) == 0 {
		return nil, observationError()
	}
	if len(journal) == 0 {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, observationError()
		}
		for _, entry := range entries {
			if entry.Name() != "proof-cleanup.runtime.lock" {
				return nil, observationError()
			}
		}
	}
	if len(key) == 0 {
		s.cipher, err = credential.Open(filepath.Join(dir, "proof-cleanup.key"), "")
	} else {
		s.cipher, err = credential.Open("", string(key))
	}
	if err != nil {
		return nil, observationError()
	}
	if len(journal) > 0 {
		plain, err := s.cipher.Decrypt(proofCleanupPurpose, string(journal))
		if err != nil || liveTVDecode([]byte(plain), &s.state) != nil {
			return nil, observationError()
		}
	} else {
		var nonce [16]byte
		if _, err = rand.Read(nonce[:]); err != nil {
			return nil, observationError()
		}
		n := hex.EncodeToString(nonce[:])
		access.Revision, access.Proofs = 1, nil
		now := time.Now().UTC()
		s.state = proofCleanupState{Version: 1, Identity: identity, Nonce: n, ProofID: "proof-cleanup-" + n, Bundle: access, Operations: map[string]cloud.OperationReceipt{}, Writes: map[string]bool{}, Started: now, Deadline: now.Add(proofCleanupDuration)}
		if s.save() != nil {
			return nil, observationError()
		}
	}
	if !proofCleanupManifestValid(s.state) || s.state.Identity != identity || !reflect.DeepEqual(s.state.Bundle.Agent, access.Agent) || !reflect.DeepEqual(s.state.Bundle.TV, access.TV) || !s.state.Completed && !time.Now().Before(s.state.Deadline) {
		return nil, observationLocalFailure("proof_cleanup_batch_preserved_snapshot_or_deadline_changed")
	}
	ok = true
	return s, nil
}

func (s *proofCleanupStore) allowed(r cloud.OperationReceipt) bool {
	return r.Provider == cloud.ProviderQuark && r.Route == string(quark.RouteAgent) && r.AccountID == s.state.Identity.Proof.AgentAccount && (r.ID == s.state.ProofID && r.Kind == "space_proof" || r.ID == s.state.ProofID+"/upload" && r.Kind == "upload")
}

func (s *proofCleanupStore) Load(_ context.Context, id string) (cloud.OperationReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.state.Operations[id]
	if s.poisoned {
		return r, observationError()
	}
	if !ok {
		return r, cloud.ErrOperationNotFound
	}
	r.Payload = append(json.RawMessage(nil), r.Payload...)
	return r, nil
}

func (s *proofCleanupStore) Create(_ context.Context, r cloud.OperationReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.state.Operations[r.ID]; exists || r.Revision != 1 || !s.allowed(r) {
		return observationError()
	}
	r.Payload = append(json.RawMessage(nil), r.Payload...)
	s.state.Operations[r.ID] = r
	return s.save()
}

func (s *proofCleanupStore) Update(_ context.Context, expected uint64, r cloud.OperationReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, exists := s.state.Operations[r.ID]
	if !exists || old.Revision != expected || r.Revision != expected+1 || !s.allowed(r) || old.Kind != r.Kind {
		return observationError()
	}
	if r.Kind == "space_proof" {
		var a, b observationProof
		if json.Unmarshal(old.Payload, &a) != nil || json.Unmarshal(r.Payload, &b) != nil || !reflect.DeepEqual(a, b) {
			return observationError()
		}
	}
	r.Payload = append(json.RawMessage(nil), r.Payload...)
	s.state.Operations[r.ID] = r
	return s.save()
}

func (s *proofCleanupStore) credentials(context.Context) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned {
		return nil, observationError()
	}
	return json.Marshal(s.state.Bundle)
}

func (s *proofCleanupStore) persist(_ context.Context, expected uint64, raw json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := quark.ParseCredentials(raw)
	old, i := s.state.Bundle, s.state.Identity
	if err != nil || c.TV == nil || c.Agent == nil || c.Web != nil || old.Revision != expected || c.Revision != expected+1 || *c.TV != *old.TV || *c.Agent != *old.Agent || len(c.Proofs) != 1 {
		return observationError()
	}
	p := c.Proofs[0]
	r := s.state.Operations[s.state.ProofID]
	var receipt struct{ Proof *quark.SpaceProof }
	if r.State != "confirmed" || json.Unmarshal(r.Payload, &receipt) != nil || receipt.Proof == nil || p != *receipt.Proof || p.ReceiptID != s.state.ProofID || p.Path != "/"+i.RootName || p.TVRoot != i.Proof.TVRoot || p.TVAccount != i.Proof.TVAccount || p.AgentAccount != i.Proof.AgentAccount {
		return observationError()
	}
	want, ok := followRawID(i.Proof.AgentRoot, quark.RouteAgent, p.AgentAccount)
	got, valid := followRawID(p.AgentRoot, quark.RouteAgent, p.AgentAccount)
	if !ok || !valid || !quark.ObservationSameAgentHandle(want, got) {
		return observationError()
	}
	s.state.Bundle = c
	return s.save()
}
