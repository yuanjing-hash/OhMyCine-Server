//go:build quark_live

package handlers_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

const followPurpose = "quark-native-acceptance:observation-followon:v1"

// Reserve the one-byte oversized-response sentinel too, before media dispatch.
const followReadLimit = observationFixtureSize + 1

type followMapping struct {
	Version         int
	Operation       string
	ReceiptRevision uint64 `json:"receipt_revision"`
	ACK, Listed     string
	TV              cloud.Item
	SHA256          string
}
type followTarget struct {
	Proof       quark.SpaceProof
	Path        string
	AgentParent string `json:"agent_parent"`
	TVParent    string `json:"tv_parent"`
}
type followIdentity struct {
	JournalSHA, KeySHA, ParentNonce                                                string
	AgentProfile, AgentDevice, AgentClient, AgentAccount                           string
	AgentGeneration                                                                uint64
	AgentUpdated                                                                   time.Time
	TVProfile, TVDevice, TVAccount                                                 string
	Root, Sample                                                                   cloud.Item
	RootPath, RootAgent, SampleAgent, RootACK, SampleACK, ProofAgent, ProofRootACK string
	Proof                                                                          quark.SpaceProof
}
type followParent struct {
	store    *observationStore
	identity followIdentity
	files    map[string][]byte
}

func followDigest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func followIdentityFromState(st observationState, journal, key []byte, creationEvidence ...cloud.Item) (followIdentity, error) {
	var out followIdentity
	if !st.Completed {
		return out, observationLocalFailure("follow_parent_incomplete")
	}
	if !observationManifestValid(st) || st.FixtureSHA != observationFixtureSHA {
		return out, observationLocalFailure("follow_parent_manifest")
	}
	if st.Bundle.TV == nil || st.Bundle.Agent == nil || st.Bundle.Web != nil || st.Bundle.TV.RefreshToken != "" || st.Bundle.Agent.RefreshToken != "" {
		return out, observationLocalFailure("follow_parent_access_snapshot")
	}
	for index, id := range []string{st.ProofID, st.ProofID + "/upload", st.DirectoryID, st.UploadID} {
		r, ok := st.Operations[id]
		if !ok || r.State != "confirmed" || r.Provider != cloud.ProviderQuark || r.Route != string(quark.RouteAgent) || r.AccountID != st.AgentAccount {
			return out, observationLocalFailure([]string{"follow_parent_proof_receipt", "follow_parent_proof_upload_receipt", "follow_parent_directory_receipt", "follow_parent_sample_receipt"}[index])
		}
	}
	var upload struct {
		Parent, Name, Handle, SHA256 string
		Size                         int64
		Target                       *followTarget
		Mapping                      *followMapping
		Output                       *cloud.Item
		Phase                        string
	}
	var directory followOperation
	var proof struct{ Proof *quark.SpaceProof }
	u, dir := st.Operations[st.UploadID], st.Operations[st.DirectoryID]
	if json.Unmarshal(u.Payload, &upload) != nil || upload.Target == nil {
		return out, observationLocalFailure("follow_parent_sample_payload")
	}
	if json.Unmarshal(dir.Payload, &directory) != nil {
		return out, observationLocalFailure("follow_parent_directory_json")
	}
	if directory.Target == nil {
		return out, observationLocalFailure("follow_parent_directory_target_missing")
	}
	if json.Unmarshal(st.Operations[st.ProofID].Payload, &proof) != nil || proof.Proof == nil {
		return out, observationLocalFailure("follow_parent_proof_payload")
	}
	m, d, p := upload.Mapping, directory.Mapping, *proof.Proof
	var sampleTV cloud.Item
	var sampleListed string
	if m != nil {
		if m.Version != 1 || m.Operation != u.ID || m.ACK != upload.Handle || m.SHA256 != observationFixtureSHA {
			return out, observationLocalFailure("follow_parent_sample_mapping")
		}
		if m.ReceiptRevision != u.Revision {
			return out, observationLocalFailure("follow_parent_sample_mapping_revision")
		}
		sampleTV, sampleListed = m.TV, m.Listed
	} else {
		// Same-namespace upload confirmation legitimately has no cross-interface
		// Mapping. Bind its real Agent output to immutable first-scan TV evidence;
		// never convert an Agent ID into a TV ID or rewrite the original receipt.
		if len(creationEvidence) != 1 || upload.Output == nil || upload.Phase != "metadata_confirmed" {
			return out, observationLocalFailure("follow_parent_sample_creation_evidence")
		}
		var valid bool
		sampleListed, valid = followRawID(upload.Output.ID, quark.RouteAgent, st.AgentAccount)
		parent, parentOK := followRawID(upload.Output.ParentID, quark.RouteAgent, st.AgentAccount)
		if !valid || !parentOK || !quark.ObservationSameAgentHandle(sampleListed, upload.Handle) || !quark.ObservationSameAgentHandle(parent, upload.Parent) || upload.Output.Name != st.UploadName || upload.Output.Size != observationFixtureSize || upload.Output.IsDir {
			return out, observationLocalFailure("follow_parent_sample_output")
		}
		sampleTV = creationEvidence[0]
	}
	for _, id := range []string{sampleTV.ID, upload.Target.TVParent, p.TVRoot} {
		if _, ok := followRawID(id, quark.RouteTV, st.TVAccount); !ok {
			return out, observationLocalFailure("follow_parent_tv_namespace")
		}
	}
	proofAgent, ok := followRawID(p.AgentRoot, quark.RouteAgent, st.AgentAccount)
	if !ok {
		return out, observationLocalFailure("follow_parent_agent_namespace")
	}
	if !reflect.DeepEqual(directory.Target.Proof, p) || !reflect.DeepEqual(upload.Target.Proof, p) {
		return out, observationLocalFailure("follow_parent_target_proof")
	}
	if directory.Target.Path != p.Path || upload.Target.Path != p.Path+"/"+st.DirectoryName || !quark.ObservationSameAgentHandle(directory.Target.AgentParent, proofAgent) {
		return out, observationLocalFailure("follow_parent_target_path")
	}
	if upload.SHA256 != observationFixtureSHA || upload.Size != observationFixtureSize || upload.Name != st.UploadName || sampleTV.Name != st.UploadName || sampleTV.Size != observationFixtureSize || sampleTV.IsDir {
		return out, observationLocalFailure("follow_parent_sample_content")
	}
	var root cloud.Item
	var rootAgent string
	if d != nil {
		if _, valid := followRawID(d.TV.ID, quark.RouteTV, st.TVAccount); !valid {
			return out, observationLocalFailure("follow_parent_directory_namespace")
		}
		if d.Version != 1 || d.Operation != dir.ID || d.ACK != directory.Handle || !d.TV.IsDir || d.TV.Name != st.DirectoryName {
			return out, observationLocalFailure("follow_parent_directory_mapping")
		}
		if d.ReceiptRevision != dir.Revision {
			return out, observationLocalFailure("follow_parent_directory_mapping_revision")
		}
		root, rootAgent = d.TV, d.Listed
	} else {
		// Native mkdir deliberately omits Mapping when ACK and listed identities
		// are in the same namespace. Require its actual confirmed output/parent,
		// then cross-bind the TV root with the independently verified upload's
		// frozen target and file mapping. No directory is adopted by name.
		if !followSameDirectoryOutput(dir, directory, st.AgentAccount, proofAgent, st.DirectoryName) || !quark.ObservationSameAgentHandle(directory.Handle, upload.Parent) {
			return out, observationLocalFailure("follow_parent_same_domain_output")
		}
		root = cloud.Item{ID: upload.Target.TVParent, ParentID: p.TVRoot, Name: st.DirectoryName, IsDir: true}
		rootAgent = upload.Parent
	}
	if sampleTV.ParentID != root.ID || upload.Target.TVParent != root.ID || root.ParentID != p.TVRoot || directory.Target.TVParent != p.TVRoot {
		return out, observationLocalFailure("follow_parent_tv_parent")
	}
	if upload.Target.AgentParent != upload.Parent || !quark.ObservationSameAgentHandle(upload.Parent, rootAgent) {
		return out, observationLocalFailure("follow_parent_agent_parent")
	}
	if p.ReceiptID != st.ProofID || p.Path != "/"+st.RootName || !st.Bundle.HasProof(p.TVRoot, p.AgentRoot) {
		return out, observationLocalFailure("follow_parent_common_space")
	}
	for _, id := range []string{sampleTV.ID, root.ID, sampleListed, rootAgent, upload.Handle, directory.Handle, directory.Target.AgentParent, st.RootACK} {
		if !liveTVScalar(id, 4096) {
			return out, observationLocalFailure("follow_parent_scalar")
		}
	}
	sample := sampleTV
	root.PickCode, sample.PickCode = "", ""
	out = followIdentity{JournalSHA: followDigest(journal), KeySHA: followDigest(key), ParentNonce: st.Nonce, AgentProfile: st.AgentProfile, AgentDevice: st.AgentDevice, AgentClient: st.AgentClient, AgentAccount: st.AgentAccount, AgentGeneration: st.AgentGeneration, AgentUpdated: st.AgentUpdated, TVProfile: st.TVProfile, TVDevice: st.TVDevice, TVAccount: st.TVAccount, Root: root, Sample: sample, RootPath: p.Path + "/" + st.DirectoryName, RootAgent: rootAgent, SampleAgent: sampleListed, RootACK: directory.Handle, SampleACK: upload.Handle, ProofAgent: directory.Target.AgentParent, ProofRootACK: st.RootACK, Proof: p}
	return out, nil
}

func openFollowParent(dir string, a *observationAgentState, tv *liveTVStore) (*followParent, error) {
	// Require both files BEFORE reusing the original loader. It cannot create a
	// substitute batch, reset a key or rewrite the completed journal.
	journal, e := liveTVRead(filepath.Join(dir, "observation.enc"), 2<<20, false)
	if e != nil {
		return nil, observationLocalFailure("follow_parent_journal_unreadable")
	}
	key, e := liveTVRead(filepath.Join(dir, "observation.key"), 1024, false)
	if e != nil {
		return nil, observationLocalFailure("follow_parent_key_unreadable")
	}
	old, e := openObservationStore(dir, a, tv)
	if e != nil {
		return nil, observationLocalFailure("follow_parent_store_binding")
	}
	id, e := followIdentityFromState(old.state, journal, key)
	if e == observationLocalFailure("follow_parent_sample_creation_evidence") {
		var upload struct{ Target *followTarget }
		if json.Unmarshal(old.state.Operations[old.state.UploadID].Payload, &upload) == nil && upload.Target != nil {
			var sample cloud.Item
			sample, e = services.QuarkObservationCreationIdentity(context.Background(), filepath.Join(dir, "service"), old.state.TVAccount, upload.Target.TVParent, upload.Target.Path, old.state.UploadName, observationFixtureSize)
			if e == nil {
				id, e = followIdentityFromState(old.state, journal, key, sample)
			}
		}
	}
	if e != nil {
		old.lock.Close()
		return nil, e
	}
	p := &followParent{store: old, identity: id, files: map[string][]byte{filepath.Join(dir, "observation.enc"): journal, filepath.Join(dir, "observation.key"): key}}
	if p.unchanged() != nil {
		old.lock.Close()
		return nil, observationError()
	}
	return p, nil
}
func (p *followParent) unchanged() error {
	for name, original := range p.files {
		b, e := liveTVRead(name, 2<<20, false)
		if e != nil || !bytes.Equal(original, b) {
			return observationError()
		}
	}
	return nil
}

type followState struct {
	Version                                                                int
	Identity                                                               followIdentity
	Nonce, RunID, RenameName, DirectoryName, RenameID, DirectoryID, MoveID string
	Bundle                                                                 quark.Credentials
	Operations                                                             map[string]cloud.OperationReceipt
	Writes                                                                 map[string]bool
	Requests, API, BrokerWrites, Reads, ReadBytes                          int
	Started, LastDispatch                                                  time.Time
	NewDirectory                                                           cloud.Item
	NewDirectoryAgent                                                      string
	InitialContentVerified                                                 bool
	Completed                                                              bool
}
type followStore struct {
	mu       sync.Mutex
	dir      string
	cipher   *credential.Store
	lock     *database.ExclusiveRuntime
	state    followState
	poisoned bool
}

func (s *followStore) save() error {
	if s.poisoned {
		return observationError()
	}
	data, e := json.Marshal(s.state)
	if e != nil {
		return observationError()
	}
	sealed, e := s.cipher.Encrypt(followPurpose, string(data))
	if e != nil {
		s.poisoned = true
		return observationError()
	}
	f, e := os.CreateTemp(s.dir, ".followon-*.tmp")
	if e != nil {
		s.poisoned = true
		return observationError()
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if e = liveTVCopySecurity(filepath.Join(s.dir, "followon.key"), f.Name()); e == nil {
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
		e = os.Rename(f.Name(), filepath.Join(s.dir, "followon.enc"))
	}
	if e != nil {
		s.poisoned = true
		return observationError()
	}
	return nil
}
func followManifestValid(st followState) bool {
	n, e := hex.DecodeString(st.Nonce)
	return e == nil && len(n) == 16 && st.Version == 1 && st.RunID == "observation-followon-"+st.Nonce && st.RenameName == "omc-renamed-"+st.Nonce+".mp4" && st.DirectoryName == "observed-move-"+st.Nonce && st.RenameID == st.RunID+"-rename" && st.DirectoryID == st.RunID+"-mkdir" && st.MoveID == st.RunID+"-move" && st.Requests >= 0 && st.Requests <= 220 && st.API >= 0 && st.API <= 200 && st.API <= st.Requests && st.BrokerWrites >= 0 && st.BrokerWrites <= 1 && st.Reads >= 0 && st.Reads <= 4 && st.ReadBytes >= 0 && st.ReadBytes <= 4*followReadLimit && st.Operations != nil && st.Writes != nil && !st.Started.IsZero() && !st.Started.After(time.Now())
}
func openFollowStore(dir string, parent *followParent) (*followStore, error) {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) == filepath.Clean(parent.store.dir) || observationCheckPrivateDirectory(dir) != nil {
		return nil, observationError()
	}
	lock, e := database.AcquireExclusiveRuntime(filepath.Join(dir, "followon"))
	if e != nil {
		return nil, observationError()
	}
	s := &followStore{dir: dir, lock: lock}
	ok := false
	defer func() {
		if !ok {
			lock.Close()
		}
	}()
	journal, e := liveTVRead(filepath.Join(dir, "followon.enc"), 2<<20, true)
	if e != nil {
		return nil, observationError()
	}
	key, e := liveTVRead(filepath.Join(dir, "followon.key"), 1024, true)
	if e != nil || len(journal) > 0 && len(key) == 0 {
		return nil, observationError()
	}
	if len(journal) == 0 {
		entries, e := os.ReadDir(dir)
		if e != nil {
			return nil, observationError()
		}
		for _, entry := range entries {
			if entry.Name() != "followon.runtime.lock" {
				return nil, observationError()
			}
		}
	}
	if len(key) == 0 {
		s.cipher, e = credential.Open(filepath.Join(dir, "followon.key"), "")
	} else {
		s.cipher, e = credential.Open("", string(key))
	}
	if e != nil {
		return nil, observationError()
	}
	if len(journal) > 0 {
		plain, e := s.cipher.Decrypt(followPurpose, string(journal))
		if e != nil || liveTVDecode([]byte(plain), &s.state) != nil {
			return nil, observationError()
		}
	} else {
		var nonce [16]byte
		if _, e = rand.Read(nonce[:]); e != nil {
			return nil, observationError()
		}
		n := hex.EncodeToString(nonce[:])
		id := "observation-followon-" + n
		s.state = followState{Version: 1, Identity: parent.identity, Nonce: n, RunID: id, RenameName: "omc-renamed-" + n + ".mp4", DirectoryName: "observed-move-" + n, RenameID: id + "-rename", DirectoryID: id + "-mkdir", MoveID: id + "-move", Operations: map[string]cloud.OperationReceipt{}, Writes: map[string]bool{}, Started: time.Now().UTC()}
		if s.save() != nil {
			return nil, observationError()
		}
	}
	if !followManifestValid(s.state) || !reflect.DeepEqual(s.state.Identity, parent.identity) || !s.state.Completed && (time.Since(s.state.Started) >= 12*time.Minute || s.state.API >= 200 || s.state.Requests >= 220) {
		return nil, observationError()
	}
	ok = true
	return s, nil
}
func (s *followStore) Load(_ context.Context, id string) (cloud.OperationReceipt, error) {
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
func (s *followStore) allowed(r cloud.OperationReceipt) bool {
	return r.Provider == cloud.ProviderQuark && r.Route == string(quark.RouteAgent) && r.AccountID == s.state.Identity.AgentAccount && (r.ID == s.state.RenameID && r.Kind == "rename" || r.ID == s.state.DirectoryID && r.Kind == "mkdir" || r.ID == s.state.MoveID && r.Kind == "move")
}
func (s *followStore) Create(_ context.Context, r cloud.OperationReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Operations[r.ID]; ok || !s.allowed(r) || r.Revision != 1 {
		return observationError()
	}
	r.Payload = append(json.RawMessage(nil), r.Payload...)
	s.state.Operations[r.ID] = r
	return s.save()
}
func (s *followStore) Update(_ context.Context, expected uint64, r cloud.OperationReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.state.Operations[r.ID]
	if !ok || !s.allowed(r) || old.Revision != expected || r.Revision != expected+1 || old.Kind != r.Kind {
		return observationError()
	}
	r.Payload = append(json.RawMessage(nil), r.Payload...)
	s.state.Operations[r.ID] = r
	return s.save()
}
func (s *followStore) credentials(context.Context) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned {
		return nil, observationError()
	}
	return json.Marshal(s.state.Bundle)
}
func (s *followStore) count() int { s.mu.Lock(); defer s.mu.Unlock(); return s.state.Requests }
