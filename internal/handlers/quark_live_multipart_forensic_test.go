//go:build quark_live

package handlers_test

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

const multipartForensicPurpose = "quark-native-acceptance:multipart-forensic:v1"
const multipartForensicLimit = 256 << 10
const multipartForensicLifetime = 30 * time.Second

func forensicError() error { return errors.New("multipart_forensic_rejected") }

type forensicFile struct {
	SHA      string
	Size     int64
	Modified time.Time
}
type forensicOriginal struct {
	dir     string
	lock    *database.ExclusiveRuntime
	files   map[string]forensicFile
	state   multipartState
	payload multipartPayload
	prefix  [5]uint32
	body    []byte
}

func forensicRead(path string, limit int64) ([]byte, forensicFile, error) {
	var out forensicFile
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit || info.Size() < 1 {
		return nil, out, forensicError()
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, out, forensicError()
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	after, se := f.Stat()
	if err != nil || se != nil || int64(len(b)) != info.Size() || !os.SameFile(info, after) || !after.ModTime().Equal(info.ModTime()) {
		return nil, out, forensicError()
	}
	return b, forensicFile{followDigest(b), info.Size(), info.ModTime()}, nil
}
func (o *forensicOriginal) unchanged() bool {
	for name, before := range o.files {
		_, after, err := forensicRead(filepath.Join(o.dir, name), before.Size)
		if err != nil || after != before {
			return false
		}
	}
	return true
}
func openForensicOriginal(dir string) (*forensicOriginal, error) {
	if !filepath.IsAbs(dir) || observationCheckPrivateDirectory(dir) != nil {
		return nil, forensicError()
	}
	// Existing files are required before taking the existing OS-lifetime lock.
	for _, name := range []string{"multipart.key", "multipart.enc", "sample.bin"} {
		if info, err := os.Lstat(filepath.Join(dir, name)); err != nil || !info.Mode().IsRegular() {
			return nil, forensicError()
		}
	}
	lock, err := database.AcquireExclusiveRuntime(filepath.Join(dir, "multipart"))
	if err != nil {
		return nil, forensicError()
	}
	o := &forensicOriginal{dir: dir, lock: lock, files: map[string]forensicFile{}}
	ok := false
	defer func() {
		if !ok {
			lock.Close()
		}
	}()
	key, info, err := forensicRead(filepath.Join(dir, "multipart.key"), 1024)
	if err != nil {
		return nil, err
	}
	o.files["multipart.key"] = info
	raw, info, err := forensicRead(filepath.Join(dir, "multipart.enc"), 2<<20)
	if err != nil {
		return nil, err
	}
	o.files["multipart.enc"] = info
	cipher, err := credential.Open("", string(key))
	if err != nil {
		return nil, forensicError()
	}
	plain, err := cipher.Decrypt(multipartPurpose, string(raw))
	if err != nil || liveTVDecode([]byte(plain), &o.state) != nil || !multipartManifest(o.state) {
		return nil, forensicError()
	}
	st := o.state
	profile, size := multipartFixtureProfile(st)
	r, exists := st.Operations[st.UploadID]
	if !exists || r.ID != st.UploadID || r.Provider != cloud.ProviderQuark || r.Kind != "upload" || r.Route != string(quark.RouteAgent) || r.AccountID != st.Identity.AgentAccount || r.State != "accepted" || json.Unmarshal(r.Payload, &o.payload) != nil {
		return nil, forensicError()
	}
	p := o.payload
	a := st.Bundle.Agent
	if profile != multipartSmallProfile || size != multipartSmallSize || !st.RootVerified || !st.Eligible || st.Completed || st.Stopped != "" || st.Pre != 1 || st.Put != 1 || st.Finish != 0 || st.Reads != 0 || st.UploadReserved != 4<<20 || st.UploadActual != 4<<20 || p.Size != size || p.PartSize != 4<<20 || p.Phase != "put_received" || p.InFlightPart != 0 || p.Handle != "" || p.Name != st.UploadName || p.SHA256 != st.SourceSHA || !quark.ObservationSameAgentHandle(p.Parent, st.ChildAgent) || !liveTVScalar(p.TaskID, 4096) || len(p.Parts) != 1 || p.Parts[0].Number != 1 || p.Parts[0].ETag == "" || p.Parts[0].ETag != st.ETags[1] || !st.Writes["part:1"] || len(st.Destinations) != 1 || st.Destinations[1].Number != 1 {
		return nil, forensicError()
	}
	for n := 2; n <= 8; n++ {
		if st.Writes["part:"+string(rune('0'+n))] {
			return nil, forensicError()
		}
	}
	if a == nil || !a.IdentityVerified || a.RefreshToken != "" || a.AccountID != st.Identity.AgentAccount || a.DeviceID != st.Identity.AgentDevice || a.ClientDeviceID != st.Identity.AgentClient || a.Generation != st.Identity.AgentGeneration || !liveTVScalar(a.AccessToken, 32768) || st.Identity.AgentProfile != quark.ObservationAgentProfileFingerprint() {
		return nil, forensicError()
	}
	pre := inspectMultipartState(st)
	if !pre.ReceiptMatchesPre || pre.PartCount != 3 || pre.RapidUpload || pre.NextPart != 2 || pre.NextDestinationRecorded {
		return nil, forensicError()
	}
	source, info, err := forensicRead(filepath.Join(dir, "sample.bin"), size)
	if err != nil || info.Size != size || info.SHA != st.SourceSHA {
		return nil, forensicError()
	}
	o.files["sample.bin"] = info
	h := sha1.New()
	_, _ = h.Write(source[:4<<20])
	state, err := h.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil || len(state) < 24 || string(state[:4]) != "sha\x01" {
		return nil, forensicError()
	}
	for i := range o.prefix {
		o.prefix[i] = binary.BigEndian.Uint32(state[4+i*4 : 8+i*4])
	}
	o.body, _ = json.Marshal(map[string]any{"task_id": p.TaskID, "part_info_list": []any{map[string]any{"part_number": 2, "part_size": int64(4 << 20), "parallel_sha1_ctx": map[string]any{"part_offset": int64(4 << 20), "h": o.prefix}}}})
	ok = true
	return o, nil
}

type forensicState struct {
	Version                                       int
	Started                                       time.Time
	OriginalFiles                                 map[string]forensicFile
	BodySHA                                       string
	Reserved                                      bool
	ResponseSaved                                 bool
	Outcome                                       string
	HTTPStatus                                    int
	RequestBytes, ResponseReserved, ResponseBytes int64
	RawResponse                                   []byte
}
type forensicStore struct {
	mu       sync.Mutex
	dir      string
	lock     *database.ExclusiveRuntime
	cipher   *credential.Store
	state    forensicState
	poisoned bool
}

func (s *forensicStore) save() error {
	if s.poisoned {
		return forensicError()
	}
	b, err := json.Marshal(s.state)
	if err != nil {
		return forensicError()
	}
	sealed, err := s.cipher.Encrypt(multipartForensicPurpose, string(b))
	if err != nil {
		return forensicError()
	}
	f, err := os.CreateTemp(s.dir, ".forensic-*.tmp")
	if err != nil {
		return forensicError()
	}
	defer os.Remove(f.Name())
	defer f.Close()
	err = liveTVCopySecurity(filepath.Join(s.dir, "forensic.key"), f.Name())
	if err == nil {
		err = f.Chmod(0600)
	}
	if err == nil {
		_, err = f.WriteString(sealed)
	}
	if err == nil {
		err = f.Sync()
	}
	if ce := f.Close(); err == nil {
		err = ce
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(s.dir, "forensic.enc"))
	}
	if err != nil {
		s.poisoned = true
		return forensicError()
	}
	return nil
}
func openForensicStore(dir string, o *forensicOriginal, inspect bool) (*forensicStore, error) {
	if !filepath.IsAbs(dir) || observationCheckPrivateDirectory(dir) != nil {
		return nil, forensicError()
	}
	for _, pair := range [][2]string{{dir, o.dir}, {o.dir, dir}} {
		rel, err := filepath.Rel(pair[0], pair[1])
		if err != nil || rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, forensicError()
		}
	}
	lock, err := database.AcquireExclusiveRuntime(filepath.Join(dir, "forensic"))
	if err != nil {
		return nil, forensicError()
	}
	s := &forensicStore{dir: dir, lock: lock}
	ok := false
	defer func() {
		if !ok {
			lock.Close()
		}
	}()
	raw, err := liveTVRead(filepath.Join(dir, "forensic.enc"), 1<<20, true)
	if err != nil {
		return nil, forensicError()
	}
	key, err := liveTVRead(filepath.Join(dir, "forensic.key"), 1024, true)
	if err != nil || len(raw) > 0 && len(key) == 0 {
		return nil, forensicError()
	}
	if len(raw) == 0 {
		if inspect {
			return nil, forensicError()
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, forensicError()
		}
		for _, e := range entries {
			if e.Name() != "forensic.runtime.lock" {
				return nil, forensicError()
			}
		}
		s.cipher, err = credential.Open(filepath.Join(dir, "forensic.key"), "")
		if err != nil {
			return nil, forensicError()
		}
		s.state = forensicState{Version: 1, Started: time.Now().UTC(), OriginalFiles: o.files, BodySHA: followDigest(o.body), Outcome: "prepared"}
		if s.save() != nil {
			return nil, forensicError()
		}
	} else {
		s.cipher, err = credential.Open("", string(key))
		if err != nil {
			return nil, forensicError()
		}
		plain, err := s.cipher.Decrypt(multipartForensicPurpose, string(raw))
		if err != nil || liveTVDecode([]byte(plain), &s.state) != nil {
			return nil, forensicError()
		}
	}
	st := s.state
	if st.Version != 1 || st.Started.IsZero() || st.Started.After(time.Now()) || !reflect.DeepEqual(st.OriginalFiles, o.files) || st.BodySHA != followDigest(o.body) || st.RequestBytes < 0 || st.RequestBytes > 4096 || st.ResponseReserved < 0 || st.ResponseReserved > multipartForensicLimit+1 || st.ResponseBytes < 0 || st.ResponseBytes > st.ResponseReserved || int64(len(st.RawResponse)) != st.ResponseBytes || st.Reserved && (st.RequestBytes != int64(len(o.body)) || st.ResponseReserved != multipartForensicLimit+1) || !st.Reserved && (st.RequestBytes != 0 || st.ResponseReserved != 0 || st.ResponseSaved) || st.HTTPStatus < 0 || st.HTTPStatus > 599 {
		return nil, forensicError()
	}
	switch st.Outcome {
	case "prepared", "reserved", "response", "network_error", "timeout", "response_oversized", "response_read_error":
	default:
		return nil, forensicError()
	}
	ok = true
	return s, nil
}

type forensicURLShape struct {
	PartNumberType       string `json:"part_number_type"`
	PartNumber           *int   `json:"part_number,omitempty"`
	PartNumberOutOfRange bool   `json:"part_number_out_of_range"`
	URLType              string `json:"url_type"`
	URLValidation        string `json:"url_validation"`
	SignatureInfoType    string `json:"signature_info_type"`
	SignatureType        string `json:"signature_type"`
	SignatureValid       bool   `json:"signature_valid"`
}
type forensicReport struct {
	Version            int                `json:"version"`
	Outcome            string             `json:"outcome"`
	RequestsThisRun    int                `json:"requests_this_run"`
	RequestReserved    bool               `json:"request_reserved"`
	OriginalUnchanged  bool               `json:"original_unchanged"`
	ResponseEncrypted  bool               `json:"response_encrypted"`
	HTTPStatus         int                `json:"http_status"`
	ResponseBytes      int64              `json:"response_bytes"`
	RootType           string             `json:"root_type"`
	StatusType         string             `json:"status_type"`
	Status             *int32             `json:"status,omitempty"`
	ErrnoType          string             `json:"errno_type"`
	Errno              *int32             `json:"errno,omitempty"`
	DataType           string             `json:"data_type"`
	URLsType           string             `json:"urls_type"`
	URLCount           int                `json:"url_count"`
	URLCountCapped     bool               `json:"url_count_capped"`
	ExactRequestedPart bool               `json:"exact_requested_part"`
	CommonHeadersType  string             `json:"common_headers_type"`
	URLs               []forensicURLShape `json:"urls"`
}

func forensicJSONType(raw json.RawMessage) string {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 {
		return "missing"
	}
	if !json.Valid(b) {
		return "invalid"
	}
	switch b[0] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	default:
		return "number"
	}
}
func forensicURLValidation(raw json.RawMessage) string {
	if forensicJSONType(raw) != "string" {
		return "not_string"
	}
	var value string
	_ = json.Unmarshal(raw, &value)
	if len(value) == 0 || len(value) > 32768 {
		return "invalid_length"
	}
	u, err := url.Parse(value)
	if err != nil {
		return "invalid_url"
	}
	if u.Scheme != "https" {
		return "non_https"
	}
	if u.Host == "" || u.Hostname() == "" {
		return "missing_host"
	}
	if u.User != nil {
		return "userinfo"
	}
	if u.Fragment != "" {
		return "fragment"
	}
	return "valid"
}
func forensicDescribe(st forensicState) forensicReport {
	r := forensicReport{Version: 1, Outcome: st.Outcome, RequestReserved: st.Reserved, ResponseEncrypted: st.ResponseSaved, HTTPStatus: st.HTTPStatus, ResponseBytes: st.ResponseBytes, URLs: []forensicURLShape{}, RootType: forensicJSONType(st.RawResponse)}
	if r.RootType != "object" {
		return r
	}
	var root map[string]json.RawMessage
	_ = json.Unmarshal(st.RawResponse, &root)
	r.StatusType, r.ErrnoType, r.DataType = forensicJSONType(root["status"]), forensicJSONType(root["errno"]), forensicJSONType(root["data"])
	var n int32
	if r.StatusType == "number" && json.Unmarshal(root["status"], &n) == nil {
		v := n
		r.Status = &v
	}
	if r.ErrnoType == "number" && json.Unmarshal(root["errno"], &n) == nil {
		v := n
		r.Errno = &v
	}
	if r.DataType != "object" {
		return r
	}
	var data map[string]json.RawMessage
	_ = json.Unmarshal(root["data"], &data)
	r.URLsType, r.CommonHeadersType = forensicJSONType(data["upload_urls"]), forensicJSONType(data["common_headers"])
	if r.URLsType != "array" {
		return r
	}
	var urls []json.RawMessage
	_ = json.Unmarshal(data["upload_urls"], &urls)
	r.URLCount = min(len(urls), 32)
	r.URLCountCapped = len(urls) > 32
	for _, value := range urls[:min(len(urls), 8)] {
		var obj map[string]json.RawMessage
		_ = json.Unmarshal(value, &obj)
		entry := forensicURLShape{PartNumberType: forensicJSONType(obj["part_number"]), URLType: forensicJSONType(obj["upload_url"]), URLValidation: forensicURLValidation(obj["upload_url"]), SignatureInfoType: forensicJSONType(obj["signature_info"])}
		var number int
		if entry.PartNumberType == "number" && json.Unmarshal(obj["part_number"], &number) == nil {
			if number >= 1 && number <= 8 {
				entry.PartNumber = &number
			} else {
				entry.PartNumberOutOfRange = true
			}
		}
		var signature map[string]json.RawMessage
		_ = json.Unmarshal(obj["signature_info"], &signature)
		entry.SignatureType = forensicJSONType(signature["signature"])
		var secret string
		if json.Unmarshal(signature["signature"], &secret) == nil {
			entry.SignatureValid = liveTVScalar(secret, 8192)
		}
		r.URLs = append(r.URLs, entry)
	}
	r.ExactRequestedPart = len(urls) == 1 && len(r.URLs) == 1 && r.URLs[0].PartNumber != nil && *r.URLs[0].PartNumber == 2
	return r
}

type forensicTransport struct {
	base       http.RoundTripper
	s          *forensicStore
	o          *forensicOriginal
	media      bool
	dispatched int
}

func (w *forensicTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	s := w.s
	ctx, cancel := context.WithDeadline(request.Context(), s.state.Started.Add(multipartForensicLifetime))
	defer cancel()
	r := request.Clone(ctx)
	if w.media || s.poisoned || s.state.Reserved || ctx.Err() != nil || !time.Now().Before(s.state.Started.Add(multipartForensicLifetime)) || r.Method != "POST" || r.URL == nil || r.URL.Scheme != "https" || r.URL.Host != "open-api-drive.quark.cn" || r.URL.User != nil || r.URL.Fragment != "" || r.URL.Path != "/open/v1/file/get_upload_urls" || r.Body == nil {
		return nil, forensicError()
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	r.Body.Close()
	if err != nil || len(b) > 4096 || !bytes.Equal(b, w.o.body) || r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" || r.Header.Get("Proxy-Authorization") != "" {
		return nil, forensicError()
	}
	a := w.o.state.Bundle.Agent
	q := r.URL.Query()
	if q.Get("access_token") != a.AccessToken || q.Get("device_id") != a.DeviceID || q.Get("platform") != "web" || r.Header.Get("Authorization") != "Bearer "+a.AccessToken || r.Header.Get("X-Agent-ID") == "" || len(q) != 4 || q.Get("req_id") == "" || !w.o.unchanged() {
		return nil, forensicError()
	}
	s.state.Reserved = true
	s.state.Outcome = "reserved"
	s.state.RequestBytes = int64(len(b))
	s.state.ResponseReserved = multipartForensicLimit + 1
	if s.save() != nil {
		return nil, forensicError()
	}
	if ctx.Err() != nil || !time.Now().Before(s.state.Started.Add(multipartForensicLifetime)) {
		return nil, forensicError()
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	r.GetBody = nil
	w.dispatched++
	resp, err := w.base.RoundTrip(r)
	if err != nil {
		s.state.Outcome = "network_error"
		if ctx.Err() != nil {
			s.state.Outcome = "timeout"
		}
		_ = s.save()
		return nil, forensicError()
	}
	defer resp.Body.Close()
	s.state.HTTPStatus = resp.StatusCode
	raw, re := io.ReadAll(io.LimitReader(resp.Body, multipartForensicLimit+1))
	s.state.RawResponse = raw
	s.state.ResponseBytes = int64(len(raw))
	s.state.ResponseSaved = true
	s.state.Outcome = "response"
	if re != nil {
		s.state.Outcome = "response_read_error"
	}
	if ctx.Err() != nil {
		s.state.Outcome = "timeout"
	}
	if len(raw) > multipartForensicLimit {
		s.state.Outcome = "response_oversized"
	}
	if s.save() != nil {
		return nil, forensicError()
	}
	// All response evidence is already encrypted. No caller receives a usable
	// destination or upload receipt; terminate this metadata-only path here.
	return nil, forensicError()
}

func runMultipartForensic(ctx context.Context, original, dir string, inspect bool, replace func(http.RoundTripper) http.RoundTripper) (report forensicReport, err error) {
	o, err := openForensicOriginal(original)
	if err != nil {
		return report, forensicError()
	}
	defer o.lock.Close()
	defer func() {
		report.OriginalUnchanged = o.unchanged()
		if !report.OriginalUnchanged {
			err = forensicError()
		}
	}()
	s, err := openForensicStore(dir, o, inspect)
	if err != nil {
		return report, forensicError()
	}
	defer s.lock.Close()
	if inspect || s.state.Reserved {
		report = forensicDescribe(s.state)
		return report, nil
	}
	if !time.Now().Before(s.state.Started.Add(multipartForensicLifetime)) {
		report = forensicDescribe(s.state)
		report.Outcome = "diagnostic_expired"
		return report, nil
	}
	a := *o.state.Bundle.Agent
	a.RefreshToken = ""
	bundle := quark.Credentials{Version: 1, Agent: &a}
	raw, _ := json.Marshal(bundle)
	var transport *forensicTransport
	client, err := quark.NewObservationValidationClient(cloud.Config{Credentials: raw}, func(base http.RoundTripper, media bool) http.RoundTripper {
		if tr, ok := base.(*http.Transport); ok {
			clone := tr.Clone()
			clone.MaxResponseHeaderBytes = 64 << 10
			base = clone
		}
		if replace != nil {
			base = replace(base)
		}
		w := &forensicTransport{base: base, s: s, o: o, media: media}
		if !media {
			transport = w
		}
		return w
	})
	if err != nil {
		return report, forensicError()
	}
	_ = client.ForensicMultipartSecondURL(ctx, o.payload.TaskID, o.prefix)
	if s.poisoned {
		return report, forensicError()
	}
	report = forensicDescribe(s.state)
	report.RequestsThisRun = transport.dispatched
	return report, nil
}
func TestQuarkLiveMultipartForensic(t *testing.T) {
	original, dir := os.Getenv("OMC_QUARK_FORENSIC_ORIGINAL"), os.Getenv("OMC_QUARK_FORENSIC_STATE")
	if original == "" || dir == "" {
		t.Skip("explicit original and independent forensic directory required")
	}
	cache, cacheErr := os.UserCacheDir()
	if cacheErr != nil || filepath.Clean(dir) != filepath.Join(cache, "OhMyCine-QuarkMultipartForensic-20261022") {
		t.Fatal("fixed_forensic_directory_required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), multipartForensicLifetime)
	defer cancel()
	r, err := runMultipartForensic(ctx, original, dir, os.Getenv("OMC_QUARK_FORENSIC_INSPECT") == "1", nil)
	if err != nil {
		t.Fatal("forensic_rejected_preserve_all_original_evidence")
	}
	raw, _ := json.Marshal(r)
	t.Log(string(raw))
}
