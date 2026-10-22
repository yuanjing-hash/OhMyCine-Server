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
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/nodeclient"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

const nodeProbeSize int64 = (8 << 20) + 1
const nodeProbeLifetime = 15 * time.Minute
const nodeProbeDirectory = "D:/Development/Code/OhMyCine/.build/quark-native-integration/validation-private/quark-node-small-20261022"
const nodeProbeProfile = "node-small-8m-v1"
const nodeProbeAPIBudget = 256
const nodeProbeRequestBudget = 320
const nodeProbeMetadataBudget int64 = 96 << 20

func nodeProbeError() error { return errors.New("quark_node_probe_rejected") }

// The parent owns controller and source; only the child owns guard and the Node
// store while running. Neither side opens the other's live SQLite database.
type nodeProbeSpec struct {
	Version                                              int
	Profile, Nonce, DirectoryName, UploadName, SourceSHA string
	Started                                              time.Time
	Identity                                             followIdentity
}

func (s nodeProbeSpec) nodeID() string      { return "quark-validation-node-" + s.Nonce }
func (s nodeProbeSpec) serverID() string    { return "quark-validation-server-" + s.Nonce }
func (s nodeProbeSpec) taskID() string      { return "quark-validation-task-" + s.Nonce }
func (s nodeProbeSpec) exportID() string    { return "quark-validation-export-" + s.Nonce }
func (s nodeProbeSpec) operationID() string { return "quark-validation-upload-" + s.Nonce }
func (s nodeProbeSpec) requestID() string   { return "quark-validation-request-" + s.Nonce }
func (s nodeProbeSpec) storageID() string   { return "quark-validation-storage-" + s.Nonce }
func (s nodeProbeSpec) valid() bool {
	n, e := hex.DecodeString(s.Nonce)
	return e == nil && len(n) == 16 && s.Version == 1 && s.Profile == nodeProbeProfile && s.DirectoryName == "omc-node-"+s.Nonce && s.UploadName == "synthetic-"+s.Nonce+".bin" && nodeprotocol.ValidDigest(s.SourceSHA) && !s.Started.IsZero() && !s.Started.After(time.Now()) && s.Identity.Proof.TVRoot != "" && s.Identity.Proof.AgentRoot != ""
}

type nodeProbeController struct {
	Spec                                           nodeProbeSpec
	CertificatePEM, PrivateKeyPEM, EnrollmentToken string
	Enrolled                                       nodeclient.EnrollmentIdentity
	Plan                                           nodeprotocol.OperationPlan
	PlanDigest                                     string
	Request                                        nodeprotocol.StorageActionRequest
	Result                                         nodeprotocol.StorageActionResponse
	Envelope                                       nodeprotocol.CredentialGrantEnvelope
	Prepared, Completed                            bool
}
type nodeProbeJSON struct {
	dir, purpose string
	cipher       *credential.Store
	lock         *database.ExclusiveRuntime
}

func openNodeProbeJSON(dir, purpose string, out any) (*nodeProbeJSON, bool, error) {
	if observationCheckPrivateDirectory(dir) != nil {
		return nil, false, nodeProbeError()
	}
	l, e := database.AcquireExclusiveRuntime(filepath.Join(dir, "probe"))
	if e != nil {
		return nil, false, nodeProbeError()
	}
	s := &nodeProbeJSON{dir: dir, purpose: purpose, lock: l}
	ok := false
	defer func() {
		if !ok {
			l.Close()
		}
	}()
	raw, e := liveTVRead(filepath.Join(dir, "state.enc"), 4<<20, true)
	if e != nil {
		return nil, false, nodeProbeError()
	}
	key, e := liveTVRead(filepath.Join(dir, "state.key"), 1024, true)
	if e != nil {
		return nil, false, nodeProbeError()
	}
	fresh := len(raw) == 0 && len(key) == 0
	if fresh {
		entries, e := os.ReadDir(dir)
		if e != nil {
			return nil, false, nodeProbeError()
		}
		for _, v := range entries {
			if v.Name() != "probe.runtime.lock" {
				return nil, false, nodeProbeError()
			}
		}
		s.cipher, e = credential.Open(filepath.Join(dir, "state.key"), "")
	} else {
		if len(raw) == 0 || len(key) == 0 {
			return nil, false, nodeProbeError()
		}
		s.cipher, e = credential.Open("", string(key))
		if e != nil {
			return nil, false, nodeProbeError()
		}
		plain, de := s.cipher.Decrypt(purpose, string(raw))
		if de != nil || liveTVDecode([]byte(plain), out) != nil {
			return nil, false, nodeProbeError()
		}
	}
	if e != nil {
		return nil, false, nodeProbeError()
	}
	ok = true
	return s, fresh, nil
}
func (s *nodeProbeJSON) save(v any) error {
	raw, e := json.Marshal(v)
	if e != nil || len(raw) > 2<<20 {
		return nodeProbeError()
	}
	sealed, e := s.cipher.Encrypt(s.purpose, string(raw))
	if e != nil {
		return nodeProbeError()
	}
	f, e := os.CreateTemp(s.dir, ".probe-*.tmp")
	if e != nil {
		return nodeProbeError()
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if liveTVCopySecurity(filepath.Join(s.dir, "state.key"), f.Name()) != nil {
		return nodeProbeError()
	}
	if e = f.Chmod(0600); e == nil {
		_, e = io.WriteString(f, sealed)
	}
	if e == nil {
		e = f.Sync()
	}
	if closeErr := f.Close(); e == nil {
		e = closeErr
	}
	if e == nil {
		e = os.Rename(f.Name(), filepath.Join(s.dir, "state.enc"))
	}
	if e != nil {
		return nodeProbeError()
	}
	return nil
}
func nodeProbePrepareController(root string, identity followIdentity) (*nodeProbeJSON, *nodeProbeController, error) {
	for _, name := range []string{"controller", "guard", "source"} {
		if e := os.MkdirAll(filepath.Join(root, name), 0700); e != nil {
			return nil, nil, nodeProbeError()
		}
	}
	c := new(nodeProbeController)
	s, fresh, e := openNodeProbeJSON(filepath.Join(root, "controller"), "quark-node-probe:controller:v1", c)
	if e != nil {
		return nil, nil, e
	}
	ok := false
	defer func() {
		if !ok {
			s.lock.Close()
		}
	}()
	if fresh {
		var n [16]byte
		if _, e = rand.Read(n[:]); e != nil {
			return nil, nil, nodeProbeError()
		}
		nonce := hex.EncodeToString(n[:])
		c.Spec = nodeProbeSpec{Version: 1, Profile: nodeProbeProfile, Nonce: nonce, DirectoryName: "omc-node-" + nonce, UploadName: "synthetic-" + nonce + ".bin", Started: time.Now().UTC(), Identity: identity}
		f, e := os.OpenFile(filepath.Join(root, "source", "sample.bin"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return nil, nil, nodeProbeError()
		}
		h := sha256.New()
		_, e = io.CopyBuffer(io.MultiWriter(f, h), io.LimitReader(rand.Reader, nodeProbeSize), make([]byte, 32<<10))
		if e == nil {
			e = f.Sync()
		}
		ce := f.Close()
		if e != nil || ce != nil {
			return nil, nil, nodeProbeError()
		}
		c.Spec.SourceSHA = hex.EncodeToString(h.Sum(nil))
		controller, e := nodeclient.GenerateIdentity(c.Spec.serverID(), time.Now())
		if e != nil {
			return nil, nil, nodeProbeError()
		}
		c.CertificatePEM, c.PrivateKeyPEM = controller.CertificatePEM, controller.PrivateKeyPEM
		var token [32]byte
		if _, e = rand.Read(token[:]); e != nil {
			return nil, nil, nodeProbeError()
		}
		c.EnrollmentToken = hex.EncodeToString(token[:])
		if s.save(c) != nil {
			return nil, nil, nodeProbeError()
		}
	}
	if !c.Spec.valid() || !reflect.DeepEqual(c.Spec.Identity, identity) {
		return nil, nil, nodeProbeError()
	}
	f, e := os.Open(filepath.Join(root, "source", "sample.bin"))
	if e != nil {
		return nil, nil, nodeProbeError()
	}
	defer f.Close()
	h := sha256.New()
	n, e := io.CopyBuffer(h, io.LimitReader(f, nodeProbeSize+1), make([]byte, 32<<10))
	if e != nil || n != nodeProbeSize || hex.EncodeToString(h.Sum(nil)) != c.Spec.SourceSHA {
		return nil, nil, nodeProbeError()
	}
	ok = true
	return s, c, nil
}

type nodeProbeBudget struct {
	Spec                                                                                    nodeProbeSpec
	Requests, API, Mkdir, Pre, Put, Finish, Queries, Reads, ReadPasses                      int
	Metadata, UploadReserved, UploadActual, ReadReserved, ReadActual, ReadOffset, Detection int64
	LastDispatch                                                                            time.Time
	Writes                                                                                  map[string]bool
	Destinations                                                                            map[int]largefileDestination
	ETags                                                                                   map[int]string
	AgentIDs, AgentParents, TVParents, TVFiles                                              map[string]bool
	ReadURLs                                                                                map[string]int64
	ChildAgent, ChildTV, Stopped                                                            string
	ReadHash                                                                                []byte
	ReadInFlight                                                                            bool
	PartCount                                                                               int
}
type nodeProbeGuard struct {
	mu      sync.Mutex
	journal *nodeProbeJSON
	state   nodeProbeBudget
	root    string
	native  cloud.OperationStore
	bundle  quark.Credentials
	delay   time.Duration
}

func openNodeProbeGuard(root string, spec nodeProbeSpec) (*nodeProbeGuard, error) {
	g := &nodeProbeGuard{root: root, delay: 2 * time.Second}
	s, fresh, e := openNodeProbeJSON(filepath.Join(root, "guard"), "quark-node-probe:guard:v1", &g.state)
	if e != nil {
		return nil, e
	}
	g.journal = s
	ok := false
	defer func() {
		if !ok {
			s.lock.Close()
		}
	}()
	if fresh {
		g.state = nodeProbeBudget{Spec: spec, Writes: map[string]bool{}, Destinations: map[int]largefileDestination{}, ETags: map[int]string{}, AgentIDs: map[string]bool{}, AgentParents: map[string]bool{}, TVParents: map[string]bool{}, TVFiles: map[string]bool{}, ReadURLs: map[string]int64{}}
		a, _ := followRawID(spec.Identity.Proof.AgentRoot, quark.RouteAgent, spec.Identity.AgentAccount)
		v, _ := followRawID(spec.Identity.Proof.TVRoot, quark.RouteTV, spec.Identity.TVAccount)
		g.state.AgentIDs[a] = true
		g.state.AgentIDs[spec.Identity.ProofRootACK] = true
		g.state.AgentParents[a] = true
		g.state.TVParents[v] = true
		if s.save(g.state) != nil {
			return nil, nodeProbeError()
		}
	}
	if !spec.valid() || !reflect.DeepEqual(g.state.Spec, spec) || !g.validState() {
		return nil, nodeProbeError()
	}
	ok = true
	return g, nil
}
func (g *nodeProbeGuard) validState() bool {
	s := g.state
	if s.PartCount != 0 && (s.PartCount < 2 || s.PartCount > 8) {
		return false
	}
	return s.Requests >= 0 && s.Requests <= nodeProbeRequestBudget && s.API >= 0 && s.API <= nodeProbeAPIBudget && s.API <= s.Requests && s.Metadata >= 0 && s.Metadata <= nodeProbeMetadataBudget && s.Mkdir >= 0 && s.Mkdir <= 1 && s.Pre >= 0 && s.Pre <= 1 && s.Put >= 0 && s.Put <= 8 && s.Finish >= 0 && s.Finish <= 1 && s.Queries >= 0 && s.Queries <= 2 && s.Reads >= 0 && s.Reads <= 6 && s.ReadPasses >= 0 && s.ReadPasses <= 3 && s.ReadOffset >= 0 && s.ReadOffset < nodeProbeSize && s.UploadActual >= 0 && s.UploadActual <= s.UploadReserved && s.UploadReserved <= nodeProbeSize && s.ReadActual >= 0 && s.ReadActual <= s.ReadReserved && s.ReadReserved <= 3*nodeProbeSize && s.Detection >= 0 && s.Detection <= 6 && len(s.ReadHash) <= 256 && s.Writes != nil && s.Destinations != nil && s.ETags != nil && s.AgentIDs != nil && s.AgentParents != nil && s.TVParents != nil && s.TVFiles != nil && s.ReadURLs != nil
}
func nodeProbeNativeID(operation, kind, parent, name string) string {
	h := sha256.Sum256([]byte(operation + "\x00" + kind + "\x00" + parent + "\x00" + name))
	return "node-" + kind + "-" + hex.EncodeToString(h[:])
}
func (g *nodeProbeGuard) receipt(ctx context.Context, kind string) (cloud.OperationReceipt, error) {
	parent, name := g.state.Spec.Identity.Proof.TVRoot, g.state.Spec.DirectoryName
	if kind == "upload" {
		if g.state.ChildTV == "" {
			return cloud.OperationReceipt{}, nodeProbeError()
		}
		parent = quark.QualifyID(quark.RouteTV, g.state.Spec.Identity.TVAccount, g.state.ChildTV)
		name = g.state.Spec.UploadName
	}
	if g.native == nil {
		return cloud.OperationReceipt{}, nodeProbeError()
	}
	r, e := g.native.Load(ctx, nodeProbeNativeID(g.state.Spec.operationID(), kind, parent, name))
	if e != nil || r.Provider != cloud.ProviderQuark || r.Route != string(quark.RouteAgent) || r.AccountID != g.state.Spec.Identity.AgentAccount || r.Kind != kind {
		return cloud.OperationReceipt{}, nodeProbeError()
	}
	return r, nil
}
func (g *nodeProbeGuard) bind(c cloud.Config) error {
	b, e := quark.ParseCredentials(c.Credentials)
	i := g.state.Spec.Identity
	if e != nil || b.TV == nil || b.Agent == nil || b.Web != nil || b.TV.RefreshToken != "" || b.Agent.RefreshToken != "" || b.TV.AccountID != i.TVAccount || b.Agent.AccountID != i.AgentAccount || len(b.Proofs) != 1 || !reflect.DeepEqual(b.Proofs[0], i.Proof) {
		return nodeProbeError()
	}
	g.bundle = b
	g.native = c.Operations
	return nil
}
func nodeProbeSourceUnchanged(root string, spec nodeProbeSpec) bool {
	f, e := os.Open(filepath.Join(root, "source", "sample.bin"))
	if e != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	n, e := io.CopyBuffer(h, io.LimitReader(f, nodeProbeSize+1), make([]byte, 32<<10))
	return e == nil && n == nodeProbeSize && bytes.Equal([]byte(hex.EncodeToString(h.Sum(nil))), []byte(spec.SourceSHA))
}
