//go:build quark_live

package handlers_test

import (
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
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

// This profile is deliberately independent of every earlier multipart batch.
// Its limits, source identity, deadline and reservations never reset on resume.
const largefileSize int64 = 2*1024*1024*1024 + 1024*1024
const largefileReadChunk int64 = 8 << 20
const largefileMaxReads = int((largefileSize+largefileReadChunk-1)/largefileReadChunk) * 3
const largefileMaxParts = 1024
const largefileMaxAPI = 1400
const largefileMaxRequests = 2300
const largefileMetadataLimit int64 = 384 << 20
const largefileStandardProfile = "large-2g-plus1m-bounded8m-v1"
const largefileDirectory = "D:/Development/Code/OhMyCine/.build/quark-native-integration/validation-private/large-file-20261022"
const largefilePurpose = "quark-native-acceptance:largefile:v1"
const largefileLifetime = 120 * time.Minute

func largefileError() error { return errors.New("largefile_guard_or_state_rejected") }
func largefileProfileSize(profile string) int64 {
	if profile == largefileStandardProfile {
		return largefileSize
	}
	return 0
}
func largefileFixtureProfile(st largefileState) (string, int64) {
	if st.FixtureProfile != largefileStandardProfile || st.FixtureBytes != largefileSize {
		return "", 0
	}
	return st.FixtureProfile, st.FixtureBytes
}
func largefileStateSize(st largefileState) int64 { _, size := largefileFixtureProfile(st); return size }
func largefileMetadataBudget(st largefileState) int64 {
	if largefileStateSize(st) == largefileSize && st.MetadataLimit == largefileMetadataLimit {
		return st.MetadataLimit
	}
	return 0
}
func largefileValidateLiveProfile(profile, dir string) error {
	if profile != largefileStandardProfile || filepath.Clean(dir) != filepath.Clean(filepath.FromSlash(largefileDirectory)) {
		return largefileError()
	}
	return nil
}

type largefileDestination struct {
	Number    int    `json:"part_number"`
	URL       string `json:"upload_url"`
	Signature struct {
		Value string `json:"signature"`
	} `json:"signature_info"`
}
type largefilePart struct {
	Number int    `json:"part_number"`
	ETag   string `json:"etag"`
}
type largefilePayload struct {
	Parent       string                 `json:"parent"`
	Name         string                 `json:"name"`
	Size         int64                  `json:"size"`
	SHA256       string                 `json:"sha256"`
	SHA1         string                 `json:"sha1"`
	MD5          string                 `json:"md5"`
	TaskID       string                 `json:"task_id"`
	PartSize     int64                  `json:"part_size"`
	Phase        string                 `json:"phase"`
	Handle       string                 `json:"handle"`
	InFlightPart int                    `json:"inflight_part"`
	Parts        []largefilePart        `json:"parts"`
	Destinations []largefileDestination `json:"upload_urls"`
	Output       *cloud.Item            `json:"output"`
}

func largefilePlan(p largefilePayload) (int, bool) {
	if p.Size != largefileSize || p.PartSize <= 0 || p.PartSize > 1<<30 || p.PartSize%64 != 0 {
		return 0, false
	}
	n := (p.Size + p.PartSize - 1) / p.PartSize
	return int(n), n >= 2 && n <= largefileMaxParts
}

type largefileState struct {
	Version                                                            int
	FixtureProfile                                                     string `json:",omitempty"`
	FixtureBytes                                                       int64  `json:",omitempty"`
	MetadataLimit                                                      int64  `json:",omitempty"`
	Identity                                                           followIdentity
	Nonce, DirectoryID, UploadID, DirectoryName, UploadName, SourceSHA string
	Started, LastDispatch                                              time.Time
	Bundle                                                             quark.Credentials
	Operations                                                         map[string]cloud.OperationReceipt
	Writes                                                             map[string]bool
	Destinations                                                       map[int]largefileDestination
	ETags                                                              map[int]string
	AgentIDs, AgentParents, TVParents, TVFiles                         map[string]bool
	ReadURLs                                                           map[string]int64
	ChildAgent, ChildTV                                                string
	RootVerified, Eligible, Completed                                  bool
	Stopped                                                            string
	Requests, API, Pre, Finish, Put, Mkdir, Queries, Reads             int
	UploadReserved, UploadActual, ReadReserved, ReadActual, Metadata   int64
	PreResponse                                                        json.RawMessage
	ReadOffset                                                         int64
	SourceReady                                                        bool
	ReadPasses                                                         int
	ReadHashState                                                      []byte
	ReadInFlight                                                       bool
	ReadDetectionReserved, ReadDetectionActual                         int64
}
type largefileStore struct {
	mu       sync.Mutex
	dir      string
	cipher   *credential.Store
	lock     *database.ExclusiveRuntime
	state    largefileState
	poisoned bool
	cancel   context.CancelFunc
}

func (s *largefileStore) save() error {
	if s.poisoned {
		return largefileError()
	}
	raw, e := json.Marshal(s.state)
	if e != nil || len(raw) > 32<<20 {
		return largefileError()
	}
	sealed, e := s.cipher.Encrypt(largefilePurpose, string(raw))
	if e != nil {
		return largefileError()
	}
	f, e := os.CreateTemp(s.dir, ".largefile-*.tmp")
	if e != nil {
		return largefileError()
	}
	defer os.Remove(f.Name())
	defer f.Close()
	e = liveTVCopySecurity(filepath.Join(s.dir, "largefile.key"), f.Name())
	if e == nil {
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
		e = os.Rename(f.Name(), filepath.Join(s.dir, "largefile.enc"))
	}
	if e != nil {
		s.poisoned = true
		return largefileError()
	}
	return nil
}
func largefileManifest(st largefileState) bool {
	size := largefileStateSize(st)
	metadataBudget := largefileMetadataBudget(st)
	n, e := hex.DecodeString(st.Nonce)
	return size > 0 && metadataBudget > 0 && st.ReadDetectionReserved >= 0 && st.ReadDetectionReserved <= int64(largefileMaxReads) && st.ReadDetectionActual >= 0 && st.ReadDetectionActual <= st.ReadDetectionReserved && st.ReadOffset >= 0 && st.ReadOffset < size && st.ReadPasses >= 0 && st.ReadPasses <= 3 && len(st.ReadHashState) <= 256 && e == nil && len(n) == 16 && st.Version == 1 && st.DirectoryID == "largefile-"+st.Nonce+"-mkdir" && st.UploadID == "largefile-"+st.Nonce+"-upload" && st.DirectoryName == "omc-largefile-"+st.Nonce && st.UploadName == "synthetic-"+st.Nonce+".bin" && (st.SourceReady && len(st.SourceSHA) == 64 || !st.SourceReady && st.SourceSHA == "") && !st.Started.IsZero() && !st.Started.After(time.Now()) && st.Operations != nil && st.Writes != nil && st.Destinations != nil && st.ETags != nil && st.AgentIDs != nil && st.AgentParents != nil && st.TVParents != nil && st.TVFiles != nil && st.ReadURLs != nil && st.Requests >= 0 && st.Requests <= largefileMaxRequests && st.API >= 0 && st.API <= largefileMaxAPI && st.API <= st.Requests && st.Pre >= 0 && st.Pre <= 1 && st.Finish >= 0 && st.Finish <= 1 && st.Mkdir >= 0 && st.Mkdir <= 1 && st.Put >= 0 && st.Put <= largefileMaxParts && st.Queries >= 0 && st.Queries <= 2 && st.Reads >= 0 && st.Reads <= largefileMaxReads && st.UploadReserved >= 0 && st.UploadReserved <= size && st.ReadReserved <= 3*size && st.ReadReserved >= 0 && st.UploadActual >= 0 && st.UploadActual <= st.UploadReserved && st.ReadActual >= 0 && st.ReadActual <= st.ReadReserved && st.Metadata >= 0 && st.Metadata <= metadataBudget
}
func openLargeFileStore(dir string, parent *followParent, profiles ...string) (*largefileStore, error) {
	profile := largefileStandardProfile
	if len(profiles) > 1 {
		return nil, largefileError()
	}
	if len(profiles) == 1 {
		profile = profiles[0]
	}
	size := largefileProfileSize(profile)
	if size == 0 {
		return nil, largefileError()
	}
	if !filepath.IsAbs(dir) || observationCheckPrivateDirectory(dir) != nil {
		return nil, largefileError()
	}
	lock, e := database.AcquireExclusiveRuntime(filepath.Join(dir, "largefile"))
	if e != nil {
		return nil, largefileError()
	}
	s := &largefileStore{dir: dir, lock: lock}
	ok := false
	defer func() {
		if !ok {
			lock.Close()
		}
	}()
	raw, e := liveTVRead(filepath.Join(dir, "largefile.enc"), 48<<20, true)
	if e != nil {
		return nil, largefileError()
	}
	key, e := liveTVRead(filepath.Join(dir, "largefile.key"), 1024, true)
	if e != nil || len(raw) > 0 && len(key) == 0 {
		return nil, largefileError()
	}
	if len(raw) == 0 {
		entries, e := os.ReadDir(dir)
		if e != nil {
			return nil, largefileError()
		}
		for _, v := range entries {
			if v.Name() != "largefile.runtime.lock" {
				return nil, largefileError()
			}
		}
		s.cipher, e = credential.Open(filepath.Join(dir, "largefile.key"), "")
		if e != nil {
			return nil, largefileError()
		}
		var nonce [16]byte
		if _, e = rand.Read(nonce[:]); e != nil {
			return nil, largefileError()
		}
		n := hex.EncodeToString(nonce[:])
		s.state = largefileState{Version: 1, FixtureProfile: profile, FixtureBytes: size, Identity: parent.identity, Nonce: n, DirectoryID: "largefile-" + n + "-mkdir", UploadID: "largefile-" + n + "-upload", DirectoryName: "omc-largefile-" + n, UploadName: "synthetic-" + n + ".bin", Started: time.Now().UTC(), Bundle: parent.store.state.Bundle, Operations: map[string]cloud.OperationReceipt{}, Writes: map[string]bool{}, Destinations: map[int]largefileDestination{}, ETags: map[int]string{}, AgentIDs: map[string]bool{}, AgentParents: map[string]bool{}, TVParents: map[string]bool{}, TVFiles: map[string]bool{}, ReadURLs: map[string]int64{}}
		s.state.MetadataLimit = largefileMetadataLimit
		if s.save() != nil {
			return nil, largefileError()
		}
	} else {
		s.cipher, e = credential.Open("", string(key))
		if e != nil {
			return nil, largefileError()
		}
		plain, e := s.cipher.Decrypt(largefilePurpose, string(raw))
		if e != nil || liveTVDecode([]byte(plain), &s.state) != nil {
			return nil, largefileError()
		}
	}
	savedProfile, savedSize := largefileFixtureProfile(s.state)
	if savedProfile != profile || savedSize != size || !largefileManifest(s.state) || !reflect.DeepEqual(s.state.Identity, parent.identity) {
		return nil, largefileError()
	}
	if s.state.SourceReady {
		f, e := s.source()
		if e != nil {
			return nil, e
		}
		f.Close()
	}
	ok = true
	return s, nil
}
func (s *largefileStore) source() (*os.File, error) {
	size := largefileStateSize(s.state)
	p := filepath.Join(s.dir, "sample.bin")
	st, e := os.Lstat(p)
	if !s.state.SourceReady || size == 0 || e != nil || !st.Mode().IsRegular() || st.Size() != size {
		return nil, largefileError()
	}
	if observationCheckPrivateDirectory(s.dir) != nil {
		return nil, largefileError()
	}
	f, e := os.Open(p)
	if e != nil {
		return nil, largefileError()
	}
	h := sha256.New()
	n, e := io.Copy(h, io.LimitReader(f, size+1))
	if e != nil || n != size || hex.EncodeToString(h.Sum(nil)) != s.state.SourceSHA {
		f.Close()
		return nil, largefileError()
	}
	if _, e = f.Seek(0, 0); e != nil {
		f.Close()
		return nil, largefileError()
	}
	return f, nil
}

func (s *largefileStore) prepareSource(ctx context.Context) error {
	if s.state.SourceReady {
		return nil
	}
	// Never replace an orphan/partial file after interruption. It needs local
	// inspection, not an automatic new source or new upload identity.
	f, err := os.OpenFile(filepath.Join(s.dir, "sample.bin"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return largefileError()
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 32<<10)
	remaining := largefileSize
	for remaining > 0 {
		if ctx.Err() != nil {
			return largefileError()
		}
		n := min(remaining, int64(len(buf)))
		if _, err = io.ReadFull(rand.Reader, buf[:n]); err != nil {
			return largefileError()
		}
		if _, err = io.MultiWriter(f, h).Write(buf[:n]); err != nil {
			return largefileError()
		}
		remaining -= n
	}
	if f.Sync() != nil || f.Close() != nil {
		return largefileError()
	}
	s.state.SourceSHA, s.state.SourceReady = hex.EncodeToString(h.Sum(nil)), true
	return s.save()
}
func (s *largefileStore) allowed(r cloud.OperationReceipt) bool {
	return r.Provider == cloud.ProviderQuark && r.Route == string(quark.RouteAgent) && r.AccountID == s.state.Identity.AgentAccount && (r.ID == s.state.DirectoryID && r.Kind == "mkdir" || r.ID == s.state.UploadID && r.Kind == "upload")
}
func (s *largefileStore) Load(_ context.Context, id string) (cloud.OperationReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned {
		return cloud.OperationReceipt{}, largefileError()
	}
	r, ok := s.state.Operations[id]
	if !ok {
		return r, cloud.ErrOperationNotFound
	}
	r.Payload = append(json.RawMessage(nil), r.Payload...)
	return r, nil
}
func (s *largefileStore) Create(_ context.Context, r cloud.OperationReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Operations[r.ID]; ok || !s.allowed(r) || r.Revision != 1 {
		return largefileError()
	}
	r.Payload = append(json.RawMessage(nil), r.Payload...)
	s.state.Operations[r.ID] = r
	return s.save()
}
func (s *largefileStore) Update(_ context.Context, expected uint64, r cloud.OperationReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.state.Operations[r.ID]
	if !ok || !s.allowed(r) || old.Kind != r.Kind || old.Revision != expected || r.Revision != expected+1 {
		return largefileError()
	}
	r.Payload = append(json.RawMessage(nil), r.Payload...)
	s.state.Operations[r.ID] = r
	if r.ID == s.state.UploadID {
		var p largefilePayload
		if json.Unmarshal(r.Payload, &p) != nil {
			return largefileError()
		}
		if p.Phase == "pre_received" || p.Phase == "finish_received" && s.state.Put == 0 {
			_, eligible := largefilePlan(p)
			s.state.Eligible = eligible && p.Size == largefileStateSize(s.state) && p.Handle == "" && len(s.state.PreResponse) > 0
			if !s.state.Eligible {
				s.state.Stopped = "largefile_not_exercised"
			}
		}
	}
	if e := s.save(); e != nil {
		return e
	}
	if s.state.Stopped != "" && s.cancel != nil {
		s.cancel()
	}
	return nil
}
