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
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

// Inspection opens only the fixed experiment's encrypted journal/key. It never
// constructs a client, opens original logins/donor DBs, or saves/resumes state.
type multipartInspection struct {
	Version                 int    `json:"version"`
	FixtureProfile          string `json:"fixture_profile"`
	SampleBytes             int64  `json:"sample_bytes"`
	MetadataBudget          int64  `json:"metadata_budget"`
	MetadataReserved        int64  `json:"metadata_reserved"`
	RapidUpload             bool   `json:"rapid_upload"`
	ActualPartBytes         int64  `json:"actual_part_bytes"`
	PartCount               int64  `json:"part_count"`
	ReceiptMatchesPre       bool   `json:"receipt_matches_pre"`
	SavedStopped            bool   `json:"saved_stopped"`
	DeadlineExpired         bool   `json:"deadline_expired"`
	Reason                  string `json:"reason"`
	InspectionCloudRequests int    `json:"inspection_cloud_requests"`
	UploadReceiptPresent    bool   `json:"upload_receipt_present"`
	UploadReceiptDecoded    bool   `json:"upload_receipt_decoded"`
	ReceiptState            string `json:"receipt_state"`
	ReceiptPhase            string `json:"receipt_phase"`
	InFlightPart            int    `json:"inflight_part"`
	InFlightPartValid       bool   `json:"inflight_part_valid"`
	ConfirmedParts          []int  `json:"confirmed_parts"`
	ReceiptPartsValid       bool   `json:"receipt_parts_valid"`
	GuardDestinationParts   []int  `json:"guard_destination_parts"`
	DestinationPartsValid   bool   `json:"destination_parts_valid"`
	DispatchedParts         []int  `json:"dispatched_parts"`
	PartETagsMatch          bool   `json:"part_etags_match"`
	NextPart                int    `json:"next_part"`
	NextDestinationRecorded bool   `json:"next_destination_recorded"`
	PartPutAttempts         int    `json:"part_put_attempts"`
	OriginalPartQueries     int    `json:"original_part_queries"`
	DiagnosticStage         string `json:"diagnostic_stage"`
	URLResponseEvidence     string `json:"url_response_evidence"`
}

func inspectMultipartState(st multipartState) multipartInspection {
	profile, size := multipartFixtureProfile(st)
	out := multipartInspection{Version: 1, FixtureProfile: profile, SampleBytes: size, MetadataBudget: multipartMetadataBudget(st), MetadataReserved: st.Metadata, SavedStopped: st.Stopped != "", DeadlineExpired: !time.Now().Before(st.Started.Add(multipartLifetime)), Reason: "pre_response_missing"}
	inspectMultipartReceipt(st, &out)
	if size == 0 {
		out.Reason = "fixture_profile_invalid"
		return out
	}
	if len(st.PreResponse) == 0 {
		return out
	}
	var pre struct {
		Finish   json.RawMessage `json:"finish"`
		PartSize *int64          `json:"part_size"`
		Task     string          `json:"task_id"`
		ID       string          `json:"fid"`
	}
	if json.Unmarshal(st.PreResponse, &pre) != nil {
		out.Reason = "pre_response_invalid"
		return out
	}
	finish := string(bytes.TrimSpace(pre.Finish))
	out.RapidUpload = finish == "true" || finish == "1"
	if pre.PartSize != nil {
		out.ActualPartBytes = *pre.PartSize
		if *pre.PartSize > 0 {
			out.PartCount = size / *pre.PartSize
			if size%*pre.PartSize != 0 {
				out.PartCount++
			}
		}
	}
	var p multipartPayload
	receipt, exists := st.Operations[st.UploadID]
	if exists && json.Unmarshal(receipt.Payload, &p) == nil && p.Size == size && p.Name == st.UploadName && p.SHA256 == st.SourceSHA {
		out.ReceiptMatchesPre = out.RapidUpload && pre.ID != "" && p.Handle == pre.ID || !out.RapidUpload && pre.Task != "" && p.TaskID == pre.Task && pre.PartSize != nil && p.PartSize == *pre.PartSize
	}
	switch {
	case out.RapidUpload:
		out.Reason = "rapid_upload"
	case pre.PartSize == nil:
		out.Reason = "part_size_missing"
	case *pre.PartSize <= 0:
		out.Reason = "part_size_nonpositive"
	case *pre.PartSize > 1<<30:
		out.Reason = "part_size_exceeds_native_limit"
	case out.PartCount > 1 && *pre.PartSize%64 != 0:
		out.Reason = "part_size_alignment_invalid"
	case out.PartCount < 2:
		out.Reason = "single_part"
	case out.PartCount > 8:
		out.Reason = "part_count_exceeds_probe_budget"
	case !out.ReceiptMatchesPre:
		out.Reason = "receipt_pre_mismatch"
	case st.Stopped != "":
		out.Reason = "eligible_but_stopped"
	case st.Completed:
		out.Reason = "completed"
	default:
		out.Reason = "eligible_recorded"
	}
	return out
}

// Print only allowlisted stages, bounded part numbers, counters and comparison
// booleans. The old ledger did not retain raw get_upload_urls responses, so an
// HTTP 200 failure cannot retrospectively be classified as a provider code or
// a response-shape failure from the receipt alone.
func inspectMultipartReceipt(st multipartState, out *multipartInspection) {
	out.ConfirmedParts, out.GuardDestinationParts, out.DispatchedParts = []int{}, []int{}, []int{}
	out.DiagnosticStage = "upload_receipt_missing"
	out.URLResponseEvidence = "not_recorded_by_original_tool"
	out.PartPutAttempts, out.OriginalPartQueries = st.Put, st.Queries
	out.DestinationPartsValid = len(st.Destinations) <= 8
	for n, destination := range st.Destinations {
		if n < 1 || n > 8 || destination.Number != n {
			out.DestinationPartsValid = false
			continue
		}
		out.GuardDestinationParts = append(out.GuardDestinationParts, n)
	}
	for n := 1; n <= 8; n++ {
		if st.Writes["part:"+strconv.Itoa(n)] {
			out.DispatchedParts = append(out.DispatchedParts, n)
		}
	}
	sort.Ints(out.GuardDestinationParts)
	r, ok := st.Operations[st.UploadID]
	out.UploadReceiptPresent = ok
	if !ok {
		return
	}
	switch r.State {
	case "prepared", "submitting", "accepted", "unknown", "confirmed":
		out.ReceiptState = r.State
	default:
		out.ReceiptState = "unrecognized"
	}
	var p multipartPayload
	if json.Unmarshal(r.Payload, &p) != nil {
		out.DiagnosticStage = "upload_receipt_decode_rejected"
		return
	}
	out.UploadReceiptDecoded = true
	switch p.Phase {
	case "prepare", "pre_inflight", "pre_received", "put_inflight", "put_received", "finish_inflight", "finish_received", "metadata_confirmed":
		out.ReceiptPhase = p.Phase
	default:
		out.ReceiptPhase = "unrecognized"
	}
	out.InFlightPartValid = p.InFlightPart >= 0 && p.InFlightPart <= 8
	if out.InFlightPartValid {
		out.InFlightPart = p.InFlightPart
	}
	out.ReceiptPartsValid = len(p.Parts) <= 8
	out.PartETagsMatch = true
	seen := map[int]bool{}
	for _, part := range p.Parts {
		if part.Number < 1 || part.Number > 8 || seen[part.Number] {
			out.ReceiptPartsValid, out.PartETagsMatch = false, false
			continue
		}
		seen[part.Number] = true
		out.ConfirmedParts = append(out.ConfirmedParts, part.Number)
		if part.ETag == "" || st.ETags[part.Number] != part.ETag || !st.Writes["part:"+strconv.Itoa(part.Number)] {
			out.PartETagsMatch = false
		}
	}
	sort.Ints(out.ConfirmedParts)
	count, valid := multipartPlan(p)
	if valid {
		for _, n := range out.ConfirmedParts {
			if n > count {
				out.ReceiptPartsValid = false
			}
		}
	}
	if valid && p.Size == multipartStateSize(st) && out.ReceiptPartsValid {
		for n := 1; n <= count; n++ {
			if !seen[n] {
				out.NextPart = n
				break
			}
		}
		_, out.NextDestinationRecorded = st.Destinations[out.NextPart]
	}
	switch out.ReceiptPhase {
	case "prepare", "pre_inflight":
		out.DiagnosticStage = "pre_acknowledgement_unresolved"
	case "pre_received":
		out.DiagnosticStage = "pre_recorded_no_part_checkpoint"
	case "put_received":
		out.DiagnosticStage = "next_part_url_or_response_unresolved"
		if !valid || p.Size != multipartStateSize(st) || !out.ReceiptPartsValid || !out.PartETagsMatch {
			out.DiagnosticStage = "receipt_parts_unresolved"
		} else if out.NextPart == 0 {
			out.DiagnosticStage = "parts_recorded_finish_not_checkpointed"
		}
	case "put_inflight":
		out.DiagnosticStage = "part_checkpoint_without_dispatch"
		if out.InFlightPartValid && st.Writes["part:"+strconv.Itoa(p.InFlightPart)] {
			out.DiagnosticStage = "part_dispatch_result_unresolved"
		}
	case "finish_inflight":
		out.DiagnosticStage = "finish_acknowledgement_unresolved"
	case "finish_received":
		out.DiagnosticStage = "finish_recorded_metadata_pending"
	case "metadata_confirmed":
		out.DiagnosticStage = "output_metadata_confirmed"
	default:
		out.DiagnosticStage = "receipt_phase_unrecognized"
	}
}
func inspectMultipartDirectory(dir string) (multipartInspection, error) {
	var out multipartInspection
	if !filepath.IsAbs(dir) || observationCheckPrivateDirectory(dir) != nil {
		return out, multipartError()
	}
	// Require the original files before obtaining the existing batch lock; this
	// path cannot initialize keys, a replacement sample, or a new experiment.
	key, e := liveTVRead(filepath.Join(dir, "multipart.key"), 1024, false)
	if e != nil {
		return out, multipartError()
	}
	raw, e := liveTVRead(filepath.Join(dir, "multipart.enc"), 2<<20, false)
	if e != nil {
		return out, multipartError()
	}
	lock, e := database.AcquireExclusiveRuntime(filepath.Join(dir, "multipart"))
	if e != nil {
		return out, multipartError()
	}
	defer lock.Close()
	current, e := liveTVRead(filepath.Join(dir, "multipart.enc"), 2<<20, false)
	if e != nil || !bytes.Equal(raw, current) {
		return out, multipartError()
	}
	cipher, e := credential.Open("", string(key))
	if e != nil {
		return out, multipartError()
	}
	plain, e := cipher.Decrypt(multipartPurpose, string(raw))
	if e != nil {
		return out, multipartError()
	}
	var st multipartState
	if liveTVDecode([]byte(plain), &st) != nil || !multipartManifest(st) {
		return out, multipartError()
	}
	out = inspectMultipartState(st)
	current, e = liveTVRead(filepath.Join(dir, "multipart.enc"), 2<<20, false)
	currentKey, ke := liveTVRead(filepath.Join(dir, "multipart.key"), 1024, false)
	if e != nil || ke != nil || !bytes.Equal(raw, current) || !bytes.Equal(key, currentKey) {
		return multipartInspection{}, multipartError()
	}
	return out, nil
}

const multipartSize int64 = 32*1024*1024 + 1
const multipartSmallSize int64 = 8*1024*1024 + 1
const multipartStandardProfile = "standard-32m-v1"
const multipartSmallProfile = "small-8m-v1"
const multipartFinalSmallProfile = "final-small-8m-metadata32-v1"
const multipartFinalDirectory = "OhMyCine-QuarkMultipartFinalValidation-20261022"
const multipartPurpose = "quark-native-acceptance:multipart:v1"
const multipartLifetime = 10 * time.Minute

func multipartError() error { return errors.New("multipart_guard_or_state_rejected") }

func multipartProfileSize(profile string) int64 {
	switch profile {
	case multipartStandardProfile:
		return multipartSize
	case multipartSmallProfile, multipartFinalSmallProfile:
		return multipartSmallSize
	default:
		return 0
	}
}

// Old journals have neither field and remain the original 32 MiB + 1 profile.
// Reading them does not migrate or rewrite their stopped state or deadline.
func multipartFixtureProfile(st multipartState) (string, int64) {
	if st.FixtureProfile == "" && st.FixtureBytes == 0 {
		return multipartStandardProfile, multipartSize
	}
	size := multipartProfileSize(st.FixtureProfile)
	if size == 0 || size != st.FixtureBytes {
		return "", 0
	}
	return st.FixtureProfile, size
}

func multipartStateSize(st multipartState) int64 {
	_, size := multipartFixtureProfile(st)
	return size
}

// Legacy/default/small profiles retain their original 8 MiB ceiling. Only a
// newly selected final-small profile with an explicit frozen budget gets the
// 32 MiB allowance; a CLI cannot raise a budget or migrate an old journal.
func multipartMetadataBudget(st multipartState) int64 {
	profile, size := multipartFixtureProfile(st)
	if size == 0 {
		return 0
	}
	if profile == multipartFinalSmallProfile {
		if st.MetadataLimit == 32<<20 {
			return st.MetadataLimit
		}
		return 0
	}
	if st.MetadataLimit != 0 {
		return 0
	}
	return 8 << 20
}

func multipartValidateLiveProfile(profile, dir string) error {
	cache, err := os.UserCacheDir()
	if err != nil || multipartProfileSize(profile) == 0 {
		return multipartError()
	}
	expected := filepath.Join(cache, multipartFinalDirectory)
	if profile == multipartFinalSmallProfile && filepath.Clean(dir) != expected || profile != multipartFinalSmallProfile && filepath.Clean(dir) == expected {
		return multipartError()
	}
	return nil
}

type multipartDestination struct {
	Number    int    `json:"part_number"`
	URL       string `json:"upload_url"`
	Signature struct {
		Value string `json:"signature"`
	} `json:"signature_info"`
}
type multipartPart struct {
	Number int    `json:"part_number"`
	ETag   string `json:"etag"`
}
type multipartPayload struct {
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
	Parts        []multipartPart        `json:"parts"`
	Destinations []multipartDestination `json:"upload_urls"`
	Output       *cloud.Item            `json:"output"`
}

func multipartPlan(p multipartPayload) (int, bool) {
	if (p.Size != multipartSize && p.Size != multipartSmallSize) || p.PartSize <= 0 || p.PartSize > 1<<30 || p.PartSize%64 != 0 {
		return 0, false
	}
	n := (p.Size + p.PartSize - 1) / p.PartSize
	return int(n), n >= 2 && n <= 8
}

type multipartState struct {
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
	Destinations                                                       map[int]multipartDestination
	ETags                                                              map[int]string
	AgentIDs, AgentParents, TVParents, TVFiles                         map[string]bool
	ReadURLs                                                           map[string]int64
	ChildAgent, ChildTV                                                string
	RootVerified, Eligible, Completed                                  bool
	Stopped                                                            string
	Requests, API, Pre, Finish, Put, Mkdir, Queries, Reads             int
	UploadReserved, UploadActual, ReadReserved, ReadActual, Metadata   int64
	PreResponse                                                        json.RawMessage
}
type multipartStore struct {
	mu       sync.Mutex
	dir      string
	cipher   *credential.Store
	lock     *database.ExclusiveRuntime
	state    multipartState
	poisoned bool
	cancel   context.CancelFunc
}

func (s *multipartStore) save() error {
	if s.poisoned {
		return multipartError()
	}
	raw, e := json.Marshal(s.state)
	if e != nil {
		return multipartError()
	}
	sealed, e := s.cipher.Encrypt(multipartPurpose, string(raw))
	if e != nil {
		return multipartError()
	}
	f, e := os.CreateTemp(s.dir, ".multipart-*.tmp")
	if e != nil {
		return multipartError()
	}
	defer os.Remove(f.Name())
	defer f.Close()
	e = liveTVCopySecurity(filepath.Join(s.dir, "multipart.key"), f.Name())
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
		e = os.Rename(f.Name(), filepath.Join(s.dir, "multipart.enc"))
	}
	if e != nil {
		s.poisoned = true
		return multipartError()
	}
	return nil
}
func multipartManifest(st multipartState) bool {
	size := multipartStateSize(st)
	metadataBudget := multipartMetadataBudget(st)
	n, e := hex.DecodeString(st.Nonce)
	return size > 0 && metadataBudget > 0 && e == nil && len(n) == 16 && st.Version == 1 && st.DirectoryID == "multipart-"+st.Nonce+"-mkdir" && st.UploadID == "multipart-"+st.Nonce+"-upload" && st.DirectoryName == "omc-multipart-"+st.Nonce && st.UploadName == "synthetic-"+st.Nonce+".bin" && len(st.SourceSHA) == 64 && !st.Started.IsZero() && !st.Started.After(time.Now()) && st.Operations != nil && st.Writes != nil && st.Destinations != nil && st.ETags != nil && st.AgentIDs != nil && st.AgentParents != nil && st.TVParents != nil && st.TVFiles != nil && st.ReadURLs != nil && st.Requests >= 0 && st.Requests <= 128 && st.API >= 0 && st.API <= 112 && st.API <= st.Requests && st.Pre >= 0 && st.Pre <= 1 && st.Finish >= 0 && st.Finish <= 1 && st.Mkdir >= 0 && st.Mkdir <= 1 && st.Put >= 0 && st.Put <= 8 && st.Queries >= 0 && st.Queries <= 2 && st.Reads >= 0 && st.Reads <= 3 && st.UploadReserved >= 0 && st.UploadReserved <= size && st.ReadReserved <= 3*size && st.ReadReserved >= 0 && st.UploadActual >= 0 && st.UploadActual <= st.UploadReserved && st.ReadActual >= 0 && st.ReadActual <= st.ReadReserved && st.Metadata >= 0 && st.Metadata <= metadataBudget
}
func openMultipartStore(dir string, parent *followParent, profiles ...string) (*multipartStore, error) {
	profile := multipartStandardProfile
	if len(profiles) > 1 {
		return nil, multipartError()
	}
	if len(profiles) == 1 {
		profile = profiles[0]
	}
	size := multipartProfileSize(profile)
	if size == 0 {
		return nil, multipartError()
	}
	if !filepath.IsAbs(dir) || observationCheckPrivateDirectory(dir) != nil {
		return nil, multipartError()
	}
	lock, e := database.AcquireExclusiveRuntime(filepath.Join(dir, "multipart"))
	if e != nil {
		return nil, multipartError()
	}
	s := &multipartStore{dir: dir, lock: lock}
	ok := false
	defer func() {
		if !ok {
			lock.Close()
		}
	}()
	raw, e := liveTVRead(filepath.Join(dir, "multipart.enc"), 2<<20, true)
	if e != nil {
		return nil, multipartError()
	}
	key, e := liveTVRead(filepath.Join(dir, "multipart.key"), 1024, true)
	if e != nil || len(raw) > 0 && len(key) == 0 {
		return nil, multipartError()
	}
	if len(raw) == 0 {
		entries, e := os.ReadDir(dir)
		if e != nil {
			return nil, multipartError()
		}
		for _, v := range entries {
			if v.Name() != "multipart.runtime.lock" {
				return nil, multipartError()
			}
		}
		s.cipher, e = credential.Open(filepath.Join(dir, "multipart.key"), "")
		if e != nil {
			return nil, multipartError()
		}
		var nonce [16]byte
		if _, e = rand.Read(nonce[:]); e != nil {
			return nil, multipartError()
		}
		n := hex.EncodeToString(nonce[:])
		s.state = multipartState{Version: 1, FixtureProfile: profile, FixtureBytes: size, Identity: parent.identity, Nonce: n, DirectoryID: "multipart-" + n + "-mkdir", UploadID: "multipart-" + n + "-upload", DirectoryName: "omc-multipart-" + n, UploadName: "synthetic-" + n + ".bin", Started: time.Now().UTC(), Bundle: parent.store.state.Bundle, Operations: map[string]cloud.OperationReceipt{}, Writes: map[string]bool{}, Destinations: map[int]multipartDestination{}, ETags: map[int]string{}, AgentIDs: map[string]bool{}, AgentParents: map[string]bool{}, TVParents: map[string]bool{}, TVFiles: map[string]bool{}, ReadURLs: map[string]int64{}}
		if profile == multipartFinalSmallProfile {
			s.state.MetadataLimit = 32 << 20
		}
		// Partial creation is intentionally not regenerated after interruption.
		f, e := os.OpenFile(filepath.Join(dir, "sample.bin"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return nil, multipartError()
		}
		h := sha256.New()
		_, e = io.CopyN(io.MultiWriter(f, h), rand.Reader, size)
		if e == nil {
			e = f.Sync()
		}
		ce := f.Close()
		if e != nil || ce != nil {
			return nil, multipartError()
		}
		s.state.SourceSHA = hex.EncodeToString(h.Sum(nil))
		if s.save() != nil {
			return nil, multipartError()
		}
	} else {
		s.cipher, e = credential.Open("", string(key))
		if e != nil {
			return nil, multipartError()
		}
		plain, e := s.cipher.Decrypt(multipartPurpose, string(raw))
		if e != nil || liveTVDecode([]byte(plain), &s.state) != nil {
			return nil, multipartError()
		}
	}
	savedProfile, savedSize := multipartFixtureProfile(s.state)
	if savedProfile != profile || savedSize != size || !multipartManifest(s.state) || !reflect.DeepEqual(s.state.Identity, parent.identity) {
		return nil, multipartError()
	}
	f, e := s.source()
	if e != nil {
		return nil, e
	}
	f.Close()
	ok = true
	return s, nil
}
func (s *multipartStore) source() (*os.File, error) {
	size := multipartStateSize(s.state)
	p := filepath.Join(s.dir, "sample.bin")
	st, e := os.Lstat(p)
	if size == 0 || e != nil || !st.Mode().IsRegular() || st.Size() != size {
		return nil, multipartError()
	}
	if observationCheckPrivateDirectory(s.dir) != nil {
		return nil, multipartError()
	}
	f, e := os.Open(p)
	if e != nil {
		return nil, multipartError()
	}
	h := sha256.New()
	n, e := io.Copy(h, io.LimitReader(f, size+1))
	if e != nil || n != size || hex.EncodeToString(h.Sum(nil)) != s.state.SourceSHA {
		f.Close()
		return nil, multipartError()
	}
	if _, e = f.Seek(0, 0); e != nil {
		f.Close()
		return nil, multipartError()
	}
	return f, nil
}
func (s *multipartStore) allowed(r cloud.OperationReceipt) bool {
	return r.Provider == cloud.ProviderQuark && r.Route == string(quark.RouteAgent) && r.AccountID == s.state.Identity.AgentAccount && (r.ID == s.state.DirectoryID && r.Kind == "mkdir" || r.ID == s.state.UploadID && r.Kind == "upload")
}
func (s *multipartStore) Load(_ context.Context, id string) (cloud.OperationReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned {
		return cloud.OperationReceipt{}, multipartError()
	}
	r, ok := s.state.Operations[id]
	if !ok {
		return r, cloud.ErrOperationNotFound
	}
	r.Payload = append(json.RawMessage(nil), r.Payload...)
	return r, nil
}
func (s *multipartStore) Create(_ context.Context, r cloud.OperationReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Operations[r.ID]; ok || !s.allowed(r) || r.Revision != 1 {
		return multipartError()
	}
	r.Payload = append(json.RawMessage(nil), r.Payload...)
	s.state.Operations[r.ID] = r
	return s.save()
}
func (s *multipartStore) Update(_ context.Context, expected uint64, r cloud.OperationReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.state.Operations[r.ID]
	if !ok || !s.allowed(r) || old.Kind != r.Kind || old.Revision != expected || r.Revision != expected+1 {
		return multipartError()
	}
	r.Payload = append(json.RawMessage(nil), r.Payload...)
	s.state.Operations[r.ID] = r
	if r.ID == s.state.UploadID {
		var p multipartPayload
		if json.Unmarshal(r.Payload, &p) != nil {
			return multipartError()
		}
		if p.Phase == "pre_received" || p.Phase == "finish_received" && s.state.Put == 0 {
			_, eligible := multipartPlan(p)
			s.state.Eligible = eligible && p.Size == multipartStateSize(s.state) && p.Handle == "" && len(s.state.PreResponse) > 0
			if !s.state.Eligible {
				s.state.Stopped = "multipart_not_exercised"
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
