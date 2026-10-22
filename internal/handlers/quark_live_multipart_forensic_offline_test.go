//go:build quark_live

package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

type forensicRoundTripFunc func(*http.Request) (*http.Response, error)

func (f forensicRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func forensicOfflineOriginal(t *testing.T, metadata ...int64) string {
	t.Helper()
	s, _ := multipartFixture(t, multipartSmallProfile)
	a := s.state.Bundle.Agent
	s.state.Identity.AgentDevice = a.DeviceID
	s.state.Identity.AgentClient = a.ClientDeviceID
	s.state.Identity.AgentGeneration = a.Generation
	s.state.Identity.AgentProfile = quark.ObservationAgentProfileFingerprint()
	p := multipartPayload{Parent: s.state.ChildAgent, Name: s.state.UploadName, Size: multipartSmallSize, SHA256: s.state.SourceSHA, TaskID: "SECRET-TASK", PartSize: 4 << 20, Phase: "put_received", Parts: []multipartPart{{Number: 1, ETag: "SECRET-ETAG"}}}
	payload, _ := json.Marshal(p)
	s.state.Operations[s.state.UploadID] = cloud.OperationReceipt{ID: s.state.UploadID, Provider: cloud.ProviderQuark, Route: string(quark.RouteAgent), Kind: "upload", AccountID: a.AccountID, State: "accepted", Revision: 4, Payload: payload}
	s.state.Eligible = true
	s.state.Pre = 1
	s.state.Put = 1
	s.state.Mkdir = 1
	s.state.Requests = 33
	s.state.API = 32
	s.state.UploadReserved = 4 << 20
	s.state.UploadActual = 4 << 20
	s.state.Writes["pre"] = true
	s.state.Writes["mkdir"] = true
	s.state.Writes["part:1"] = true
	s.state.ETags[1] = "SECRET-ETAG"
	d := multipartDestination{Number: 1, URL: "https://private.invalid/SECRET-URL"}
	d.Signature.Value = "SECRET-SIGNATURE"
	s.state.Destinations[1] = d
	s.state.PreResponse = json.RawMessage(`{"task_id":"SECRET-TASK","part_size":4194304}`)
	// An expired WRITE experiment remains expired and immutable. This tool is a
	// separately authorized one-request metadata experiment, not its renewal.
	s.state.Started = time.Now().UTC().Add(-24 * time.Hour)
	if len(metadata) == 1 {
		s.state.Metadata = metadata[0]
	}
	if s.save() != nil {
		t.Fatal("synthetic original save")
	}
	s.lock.Close()
	return s.dir
}
func forensicAssertNoSecret(t *testing.T, r forensicReport) {
	t.Helper()
	b, _ := json.Marshal(r)
	for _, secret := range []string{"SECRET", "agent-test", "agent-device", "access-test", "private.invalid", "Authorization", "task_id"} {
		if strings.Contains(string(b), secret) {
			t.Fatal("private value escaped safe report")
		}
	}
}
func forensicReadLedger(t *testing.T, dir string) forensicState {
	t.Helper()
	key, err := os.ReadFile(filepath.Join(dir, "forensic.key"))
	if err != nil {
		t.Fatal("read synthetic key")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "forensic.enc"))
	if err != nil {
		t.Fatal("read synthetic state")
	}
	c, err := credential.Open("", string(key))
	if err != nil {
		t.Fatal("synthetic cipher")
	}
	plain, err := c.Decrypt(multipartForensicPurpose, string(raw))
	var st forensicState
	if err != nil || json.Unmarshal([]byte(plain), &st) != nil {
		t.Fatal("decode synthetic ledger")
	}
	return st
}
func TestQuarkMultipartForensicOfflineOneRequestAndColdInspection(t *testing.T) {
	original, dir := forensicOfflineOriginal(t), observationOfflinePrivateDir(t)
	o, err := openForensicOriginal(original)
	if err != nil {
		t.Fatal("load synthetic original")
	}
	before := o.files
	body := append([]byte(nil), o.body...)
	o.lock.Close()
	calls := 0
	response := `{"status":0,"errno":0,"message":"SECRET-TEXT","data":{"upload_urls":[{"part_number":2,"upload_url":"https://private.invalid/SECRET-URL","signature_info":{"signature":"SECRET-SIGNATURE"}}],"common_headers":{"SECRET-HEADER":"SECRET-VALUE"}}}`
	wrap := func(http.RoundTripper) http.RoundTripper {
		return forensicRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			cold := forensicReadLedger(t, dir)
			if !cold.Reserved || cold.Outcome != "reserved" || cold.ResponseSaved || cold.ResponseReserved != multipartForensicLimit+1 {
				t.Fatal("reservation must be durable before network")
			}
			b, _ := io.ReadAll(r.Body)
			if !bytes.Equal(b, body) || r.Method != "POST" || r.URL.Host != "open-api-drive.quark.cn" || r.URL.Path != "/open/v1/file/get_upload_urls" {
				t.Fatal("exact original request only")
			}
			deadline, ok := r.Context().Deadline()
			if !ok || deadline.After(cold.Started.Add(multipartForensicLifetime)) {
				t.Fatal("persisted deadline missing")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(response))}, nil
		})
	}
	r, err := runMultipartForensic(context.Background(), original, dir, false, wrap)
	if err != nil || calls != 1 || r.RequestsThisRun != 1 || !r.OriginalUnchanged || !r.ResponseEncrypted || r.Outcome != "response" || r.Status == nil || *r.Status != 0 || !r.ExactRequestedPart || len(r.URLs) != 1 || !r.URLs[0].SignatureValid {
		t.Fatal("one metadata diagnostic")
	}
	forensicAssertNoSecret(t, r)
	st := forensicReadLedger(t, dir)
	if string(st.RawResponse) != response {
		t.Fatal("encrypted response evidence missing")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "forensic.enc"))
	if bytes.Contains(raw, []byte("SECRET")) {
		t.Fatal("plaintext response on disk")
	}
	for _, inspect := range []bool{false, true} {
		r, err = runMultipartForensic(context.Background(), original, dir, inspect, wrap)
		if err != nil || calls != 1 || r.RequestsThisRun != 0 || !r.OriginalUnchanged || !r.ExactRequestedPart {
			t.Fatal("repeat must stay local")
		}
		forensicAssertNoSecret(t, r)
	}
	o, err = openForensicOriginal(original)
	if err != nil {
		t.Fatal("original remains readable")
	}
	defer o.lock.Close()
	if !reflect.DeepEqual(before, o.files) {
		t.Fatal("original bytes or mtime changed")
	}
}
func TestQuarkMultipartForensicOfflineUnknownAndExpiry(t *testing.T) {
	for _, mode := range []string{"lost_ack", "timeout", "expired"} {
		t.Run(mode, func(t *testing.T) {
			original, dir := forensicOfflineOriginal(t), observationOfflinePrivateDir(t)
			calls := 0
			if mode == "expired" {
				o, err := openForensicOriginal(original)
				if err != nil {
					t.Fatal("original")
				}
				s, err := openForensicStore(dir, o, false)
				if err != nil {
					t.Fatal("new ledger")
				}
				s.state.Started = time.Now().UTC().Add(-time.Minute)
				if s.save() != nil {
					t.Fatal("seed diagnostic expiry")
				}
				s.lock.Close()
				o.lock.Close()
			}
			ctx := context.Background()
			var cancel context.CancelFunc
			if mode == "timeout" {
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			wrap := func(http.RoundTripper) http.RoundTripper {
				return forensicRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					if mode == "timeout" {
						<-r.Context().Done()
					}
					return nil, errors.New("SECRET-NETWORK-URL")
				})
			}
			r, err := runMultipartForensic(ctx, original, dir, false, wrap)
			if err != nil || !r.OriginalUnchanged {
				t.Fatal("bounded unknown result")
			}
			if mode == "expired" {
				if calls != 0 || r.Outcome != "diagnostic_expired" || r.RequestReserved {
					t.Fatal("expired diagnostic sent request")
				}
			} else {
				if calls != 1 || !r.RequestReserved || r.ResponseEncrypted {
					t.Fatal("unknown request reservation")
				}
			}
			forensicAssertNoSecret(t, r)
			before := calls
			r, err = runMultipartForensic(context.Background(), original, dir, false, wrap)
			if err != nil || calls != before || r.RequestsThisRun != 0 {
				t.Fatal("unknown response must not retry")
			}
		})
	}
}
func TestQuarkMultipartForensicOfflineWrongRequestRejected(t *testing.T) {
	for _, mode := range []string{"wrong_part", "wrong_task", "wrong_prefix", "put", "pre", "refresh", "tv", "media", "auth"} {
		t.Run(mode, func(t *testing.T) {
			original, dir := forensicOfflineOriginal(t), observationOfflinePrivateDir(t)
			o, err := openForensicOriginal(original)
			if err != nil {
				t.Fatal("original")
			}
			defer o.lock.Close()
			s, err := openForensicStore(dir, o, false)
			if err != nil {
				t.Fatal("ledger")
			}
			defer s.lock.Close()
			calls := 0
			body := append([]byte(nil), o.body...)
			if mode == "wrong_part" {
				body = bytes.Replace(body, []byte(`"part_number":2`), []byte(`"part_number":3`), 1)
			}
			if mode == "wrong_task" {
				body = bytes.Replace(body, []byte("SECRET-TASK"), []byte("OTHER-TASK"), 1)
			}
			if mode == "wrong_prefix" {
				body = bytes.Replace(body, []byte(`"part_offset":4194304`), []byte(`"part_offset":0`), 1)
			}
			a := o.state.Bundle.Agent
			u := "https://open-api-drive.quark.cn/open/v1/file/get_upload_urls?platform=web&req_id=synthetic&access_token=" + a.AccessToken + "&device_id=" + a.DeviceID
			r, _ := http.NewRequest("POST", u, bytes.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+a.AccessToken)
			r.Header.Set("X-Agent-ID", "synthetic")
			switch mode {
			case "put":
				r.Method = "PUT"
			case "pre":
				r.URL.Path = "/open/v1/file/upload_pre"
			case "refresh":
				r.URL.Path = "/agent/v1/oauth/access_token/rotate"
			case "tv":
				r.URL.Host = "api-tv.quark.cn"
			case "auth":
				r.Header.Set("Authorization", "Bearer WRONG")
			}
			w := &forensicTransport{s: s, o: o, media: mode == "media", base: forensicRoundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, forensicError() })}
			if _, err = w.RoundTrip(r); err == nil || calls != 0 || s.state.Reserved {
				t.Fatal("forbidden request reached reservation/dispatch")
			}
		})
	}
}
func TestQuarkMultipartForensicOfflineShapeAndLimits(t *testing.T) {
	for _, body := range []string{`{"status":-1,"errno":23018,"error_info":"SECRET","data":null}`, `{"status":"SECRET","errno":{},"data":{"upload_urls":[{"part_number":"2","upload_url":"http://private.invalid/SECRET","signature_info":{"signature":123}}]}}`, `{"data":{"upload_urls":[{"part_number":999999,"upload_url":"https://SECRET@private.invalid/","signature_info":null}]}}`, strings.Repeat("SECRET", 60<<10)} {
		t.Run("shape", func(t *testing.T) {
			original, dir := forensicOfflineOriginal(t), observationOfflinePrivateDir(t)
			calls := 0
			wrap := func(http.RoundTripper) http.RoundTripper {
				return forensicRoundTripFunc(func(*http.Request) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
				})
			}
			r, err := runMultipartForensic(context.Background(), original, dir, false, wrap)
			if err != nil || calls != 1 || !r.ResponseEncrypted || r.ResponseBytes > multipartForensicLimit+1 {
				t.Fatal("bounded encrypted response")
			}
			forensicAssertNoSecret(t, r)
			if len(body) > multipartForensicLimit && r.Outcome != "response_oversized" {
				t.Fatal("oversize response not explicit")
			}
		})
	}
}
func TestQuarkMultipartForensicOfflineOriginalChangedRejected(t *testing.T) {
	original, dir := forensicOfflineOriginal(t), observationOfflinePrivateDir(t)
	o, err := openForensicOriginal(original)
	if err != nil {
		t.Fatal("original")
	}
	s, err := openForensicStore(dir, o, false)
	if err != nil {
		t.Fatal("ledger")
	}
	s.lock.Close()
	o.lock.Close()
	path := filepath.Join(original, "sample.bin")
	info, _ := os.Stat(path)
	if os.Chtimes(path, info.ModTime(), info.ModTime().Add(time.Second)) != nil {
		t.Fatal("synthetic mtime change")
	}
	calls := 0
	_, err = runMultipartForensic(context.Background(), original, dir, false, func(http.RoundTripper) http.RoundTripper {
		return forensicRoundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, nil })
	})
	if err == nil || calls != 0 {
		t.Fatal("changed original binding accepted")
	}
}
