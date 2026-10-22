//go:build quark_live

package handlers_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

const retirementPurpose = "quark-native-acceptance:observation-retirement:v1"
const retirementDuration = 25 * time.Minute

type retirementIdentity struct {
	Creation                                                                  followIdentity
	PredecessorRunID, PredecessorJournalSHA, PredecessorKeySHA, ServiceKeySHA string
	Name, DirectoryName                                                       string
	Inside                                                                    cloud.Item
	InsideAgent                                                               string
}
type retirementParent struct {
	store    *followStore
	identity retirementIdentity
	files    map[string][]byte
}

func retirementIdentityFromState(st followState, journal, key, serviceKey []byte) (retirementIdentity, error) {
	var out retirementIdentity
	if !followManifestValid(st) || !st.Completed || !st.InitialContentVerified || len(journal) == 0 || len(key) == 0 || len(serviceKey) == 0 {
		return out, observationError()
	}
	s := &followStore{state: st}
	g := newFollowGuard(s)
	if !g.newDirectoryConfirmed() {
		return out, observationError()
	}
	for _, v := range []struct{ id, kind string }{{st.RenameID, "rename"}, {st.DirectoryID, "mkdir"}, {st.MoveID, "move"}} {
		r, p, ok := g.operation(v.id, v.kind)
		if !ok || r.State != "confirmed" || !st.Writes[v.id+":"+v.kind] {
			return out, observationError()
		}
		if v.kind == "mkdir" {
			continue
		}
		if len(p.Outputs) != 1 {
			return out, observationError()
		}
		f := p.Outputs[0]
		id, valid := followRawID(f.ID, quark.RouteAgent, st.Identity.AgentAccount)
		parent, pvalid := followRawID(f.ParentID, quark.RouteAgent, st.Identity.AgentAccount)
		item, ivalid := followRawID(p.Request.ItemID, quark.RouteAgent, st.Identity.AgentAccount)
		wantParent := st.Identity.RootAgent
		if v.kind == "move" {
			wantParent = st.NewDirectoryAgent
			target, tvalid := followRawID(p.Request.ParentID, quark.RouteAgent, st.Identity.AgentAccount)
			if !tvalid || !quark.ObservationSameAgentHandle(target, wantParent) {
				return out, observationError()
			}
		} else if p.Request.Name != st.RenameName {
			return out, observationError()
		}
		if !valid || !pvalid || !ivalid || !quark.ObservationSameAgentHandle(id, st.Identity.SampleAgent) || !quark.ObservationSameAgentHandle(item, st.Identity.SampleAgent) || !quark.ObservationSameAgentHandle(parent, wantParent) || f.Name != st.RenameName || f.IsDir || f.Size != observationFixtureSize {
			return out, observationError()
		}
	}
	if st.Identity.Root.ParentID != st.Identity.Proof.TVRoot || st.Identity.Root.ID == st.Identity.Proof.TVRoot || st.NewDirectory.ParentID != st.Identity.Root.ID {
		return out, observationError()
	}
	out = retirementIdentity{Creation: st.Identity, PredecessorRunID: st.RunID, PredecessorJournalSHA: followDigest(journal), PredecessorKeySHA: followDigest(key), ServiceKeySHA: followDigest(serviceKey), Name: st.RenameName, DirectoryName: st.DirectoryName, Inside: st.NewDirectory, InsideAgent: st.NewDirectoryAgent}
	return out, nil
}
func openRetirementParent(dir string, creation *followParent) (*retirementParent, error) {
	s, err := openExistingFollowDiagnosticStore(dir, creation)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			s.lock.Close()
		}
	}()
	journal, err := liveTVRead(filepath.Join(dir, "followon.enc"), 2<<20, false)
	if err != nil {
		return nil, observationError()
	}
	key, err := liveTVRead(filepath.Join(dir, "followon.key"), 1024, false)
	if err != nil {
		return nil, observationError()
	}
	serviceKeyPath := filepath.Join(creation.store.dir, "service", "validation.key")
	serviceKey, err := liveTVRead(serviceKeyPath, 1024, false)
	if err != nil {
		return nil, observationError()
	}
	i, err := retirementIdentityFromState(s.state, journal, key, serviceKey)
	if err != nil {
		return nil, err
	}
	p := &retirementParent{store: s, identity: i, files: map[string][]byte{filepath.Join(dir, "followon.enc"): journal, filepath.Join(dir, "followon.key"): key, serviceKeyPath: serviceKey}}
	if p.unchanged() != nil {
		return nil, observationError()
	}
	ok = true
	return p, nil
}
func (p *retirementParent) unchanged() error {
	for name, old := range p.files {
		b, e := liveTVRead(name, 2<<20, false)
		if e != nil || !bytes.Equal(b, old) {
			return observationError()
		}
	}
	return nil
}

type retirementState struct {
	Version                             int
	Identity                            retirementIdentity
	Nonce, RunID, MoveOutID, MoveBackID string
	Bundle                              quark.Credentials
	Operations                          map[string]cloud.OperationReceipt
	Writes                              map[string]bool
	Preflights                          map[string]retirementPreflight
	Requests, API, BrokerWrites         int
	Started, Deadline, LastDispatch     time.Time
	Completed                           bool
}
type retirementStore struct {
	mu       sync.Mutex
	dir      string
	cipher   *credential.Store
	lock     *database.ExclusiveRuntime
	state    retirementState
	poisoned bool
}

func (s *retirementStore) save() error {
	if s.poisoned {
		return observationError()
	}
	data, e := json.Marshal(s.state)
	if e != nil {
		return observationError()
	}
	sealed, e := s.cipher.Encrypt(retirementPurpose, string(data))
	if e != nil {
		s.poisoned = true
		return observationError()
	}
	f, e := os.CreateTemp(s.dir, ".retirement-*.tmp")
	if e != nil {
		s.poisoned = true
		return observationError()
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if e = liveTVCopySecurity(filepath.Join(s.dir, "retirement.key"), f.Name()); e == nil {
		e = f.Chmod(0600)
	}
	if e == nil {
		_, e = f.WriteString(sealed)
	}
	if e == nil {
		e = f.Sync()
	}
	if ce := f.Close(); e == nil {
		e = ce
	}
	if e == nil {
		e = os.Rename(f.Name(), filepath.Join(s.dir, "retirement.enc"))
	}
	if e != nil {
		s.poisoned = true
		return observationError()
	}
	return nil
}
func retirementManifestValid(st retirementState) bool {
	n, e := hex.DecodeString(st.Nonce)
	return e == nil && len(n) == 16 && st.Version == 1 && st.RunID == "observation-retirement-"+st.Nonce && st.MoveOutID == st.RunID+"-out" && st.MoveBackID == st.RunID+"-back" && !st.Started.IsZero() && !st.Started.After(time.Now()) && st.Deadline.Equal(st.Started.Add(retirementDuration)) && st.Requests >= 0 && st.Requests <= 220 && st.API >= 0 && st.API <= 200 && st.API <= st.Requests && st.BrokerWrites >= 0 && st.BrokerWrites <= 1 && st.Writes != nil && st.Operations != nil && st.Preflights != nil
}
func openRetirementStore(dir string, p *retirementParent) (*retirementStore, error) {
	if p == nil || p.store == nil || !p.store.state.Completed {
		return nil, observationError()
	}
	if !filepath.IsAbs(dir) || observationCheckPrivateDirectory(dir) != nil {
		return nil, observationError()
	}
	l, e := database.AcquireExclusiveRuntime(filepath.Join(dir, "retirement"))
	if e != nil {
		return nil, observationError()
	}
	ok := false
	defer func() {
		if !ok {
			l.Close()
		}
	}()
	s := &retirementStore{dir: dir, lock: l}
	journal, e := liveTVRead(filepath.Join(dir, "retirement.enc"), 2<<20, true)
	if e != nil {
		return nil, observationError()
	}
	key, e := liveTVRead(filepath.Join(dir, "retirement.key"), 1024, true)
	if e != nil || len(journal) > 0 && len(key) == 0 {
		return nil, observationError()
	}
	if len(journal) == 0 {
		entries, e := os.ReadDir(dir)
		if e != nil {
			return nil, observationError()
		}
		for _, entry := range entries {
			if entry.Name() != "retirement.runtime.lock" {
				return nil, observationError()
			}
		}
	}
	if len(key) == 0 {
		s.cipher, e = credential.Open(filepath.Join(dir, "retirement.key"), "")
	} else {
		s.cipher, e = credential.Open("", string(key))
	}
	if e != nil {
		return nil, observationError()
	}
	if len(journal) > 0 {
		plain, e := s.cipher.Decrypt(retirementPurpose, string(journal))
		if e != nil || liveTVDecode([]byte(plain), &s.state) != nil {
			return nil, observationError()
		}
	} else {
		var n [16]byte
		if _, e = rand.Read(n[:]); e != nil {
			return nil, observationError()
		}
		nonce := hex.EncodeToString(n[:])
		id := "observation-retirement-" + nonce
		now := time.Now().UTC()
		s.state = retirementState{Version: 1, Identity: p.identity, Nonce: nonce, RunID: id, MoveOutID: id + "-out", MoveBackID: id + "-back", Bundle: p.store.state.Bundle, Started: now, Deadline: now.Add(retirementDuration), Operations: map[string]cloud.OperationReceipt{}, Writes: map[string]bool{}, Preflights: map[string]retirementPreflight{}}
		if s.save() != nil {
			return nil, observationError()
		}
	}
	if !retirementManifestValid(s.state) || !reflect.DeepEqual(s.state.Identity, p.identity) || !s.state.Completed && (!time.Now().Before(s.state.Deadline) || s.state.API >= 200 || s.state.Requests >= 220) {
		return nil, observationError()
	}
	ok = true
	return s, nil
}
func (s *retirementStore) allowed(r cloud.OperationReceipt) bool {
	return r.Provider == cloud.ProviderQuark && r.Route == string(quark.RouteAgent) && r.AccountID == s.state.Identity.Creation.AgentAccount && r.Kind == "move" && (r.ID == s.state.MoveOutID || r.ID == s.state.MoveBackID)
}
func (s *retirementStore) Load(_ context.Context, id string) (cloud.OperationReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.state.Operations[id]
	if s.poisoned {
		return r, observationError()
	}
	if !ok {
		return r, os.ErrNotExist
	}
	r.Payload = append(json.RawMessage(nil), r.Payload...)
	return r, nil
}
func (s *retirementStore) Create(_ context.Context, r cloud.OperationReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Operations[r.ID]; ok || !s.allowed(r) || r.Revision != 1 {
		return observationError()
	}
	r.Payload = append(json.RawMessage(nil), r.Payload...)
	s.state.Operations[r.ID] = r
	return s.save()
}
func (s *retirementStore) Update(_ context.Context, expected uint64, r cloud.OperationReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.state.Operations[r.ID]
	if !ok || !s.allowed(r) || old.Revision != expected || r.Revision != expected+1 {
		return observationError()
	}
	r.Payload = append(json.RawMessage(nil), r.Payload...)
	s.state.Operations[r.ID] = r
	return s.save()
}
func (s *retirementStore) credentials(context.Context) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned {
		return nil, observationError()
	}
	return json.Marshal(s.state.Bundle)
}
func (s *retirementStore) count() int { s.mu.Lock(); defer s.mu.Unlock(); return s.state.Requests }
