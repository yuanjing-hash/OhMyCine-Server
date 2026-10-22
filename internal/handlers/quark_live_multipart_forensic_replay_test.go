//go:build quark_live

package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

type forensicReplayReport struct {
	Version                          int                           `json:"version"`
	NetworkRequests                  int                           `json:"network_requests"`
	Direct                           quark.MultipartForensicReplay `json:"direct_native"`
	Guarded                          quark.MultipartForensicReplay `json:"guarded_native"`
	UnknownLengthGuarded             quark.MultipartForensicReplay `json:"unknown_length_guarded_native"`
	GuardDestinationAccepted         bool                          `json:"guard_destination_accepted"`
	UnknownLengthDestinationAccepted bool                          `json:"unknown_length_destination_accepted"`
	MetadataReserved                 int64                         `json:"metadata_reserved"`
	MetadataRemaining                int64                         `json:"metadata_remaining"`
	ExactLengthReservationFits       bool                          `json:"exact_length_reservation_fits"`
	UnknownLengthReservationFits     bool                          `json:"unknown_length_reservation_fits"`
	OriginalUnchanged                bool                          `json:"original_unchanged"`
	ForensicUnchanged                bool                          `json:"forensic_unchanged"`
	ResponseHeadersReconstructed     bool                          `json:"response_headers_reconstructed"`
	GuardAdmissionSimulated          bool                          `json:"guard_admission_simulated"`
}

func forensicReplayLocal(t *testing.T, ctx context.Context, original, dir string) (out forensicReplayReport, err error) {
	t.Helper()
	out.Version = 1
	out.ResponseHeadersReconstructed = true
	out.GuardAdmissionSimulated = true
	o, err := openForensicOriginal(original)
	if err != nil {
		return out, forensicError()
	}
	defer o.lock.Close()
	s, err := openForensicStore(dir, o, true)
	if err != nil {
		return out, forensicError()
	}
	defer s.lock.Close()
	files := map[string]forensicFile{}
	for _, name := range []string{"forensic.key", "forensic.enc"} {
		_, info, e := forensicRead(filepath.Join(dir, name), 1<<20)
		if e != nil {
			return out, forensicError()
		}
		files[name] = info
	}
	defer func() {
		out.OriginalUnchanged = o.unchanged()
		out.ForensicUnchanged = true
		for name, info := range files {
			_, after, e := forensicRead(filepath.Join(dir, name), 1<<20)
			if e != nil || after != info {
				out.ForensicUnchanged = false
			}
		}
		if !out.OriginalUnchanged || !out.ForensicUnchanged {
			err = forensicError()
		}
	}()
	if !s.state.Reserved || !s.state.ResponseSaved || s.state.Outcome != "response" || len(s.state.RawResponse) > multipartForensicLimit {
		return out, forensicError()
	}
	source, e := os.Open(filepath.Join(original, "sample.bin"))
	if e != nil {
		return out, forensicError()
	}
	defer source.Close()
	a := *o.state.Bundle.Agent
	out.Direct = quark.ForensicReplayMultipartSecondURL(ctx, s.state.RawResponse, s.state.HTTPStatus, a, o.payload.TaskID, o.body, source, false, nil)
	out.MetadataReserved = o.state.Metadata
	out.MetadataRemaining = 8<<20 - o.state.Metadata
	out.ExactLengthReservationFits = int64(len(s.state.RawResponse))+1 <= out.MetadataRemaining
	out.UnknownLengthReservationFits = multipartForensicLimit+1 <= out.MetadataRemaining
	// This is a synthetic guard admission at the captured stage. Its private
	// ephemeral encrypted state is distinct from BOTH original experiments.
	// No real deadline/counter/receipt is changed or considered renewable.
	replayGuard := func(unknownLength bool) (quark.MultipartForensicReplay, bool, error) {
		cloneDir := observationOfflinePrivateDir(t)
		clone := &multipartStore{dir: cloneDir}
		sealed, _ := json.Marshal(o.state)
		if json.Unmarshal(sealed, &clone.state) != nil {
			return quark.MultipartForensicReplay{}, false, forensicError()
		}
		clone.cipher, e = credential.Open(filepath.Join(cloneDir, "multipart.key"), "")
		if e != nil {
			return quark.MultipartForensicReplay{}, false, forensicError()
		}
		clone.state.Started = time.Now().UTC()
		clone.state.LastDispatch = time.Time{}
		guard := newMultipartGuard(clone)
		guard.delay = 0
		r := quark.ForensicReplayMultipartSecondURL(ctx, s.state.RawResponse, s.state.HTTPStatus, a, o.payload.TaskID, o.body, source, unknownLength, func(base http.RoundTripper) http.RoundTripper { return guard.wrap(base, false) })
		_, accepted := clone.state.Destinations[2]
		return r, accepted, nil
	}
	out.Guarded, out.GuardDestinationAccepted, e = replayGuard(false)
	if e != nil {
		return out, e
	}
	out.UnknownLengthGuarded, out.UnknownLengthDestinationAccepted, e = replayGuard(true)
	if e != nil {
		return out, e
	}
	return out, nil
}
func TestQuarkLiveMultipartForensicReplay(t *testing.T) {
	original, dir := os.Getenv("OMC_QUARK_FORENSIC_ORIGINAL"), os.Getenv("OMC_QUARK_FORENSIC_STATE")
	if original == "" || dir == "" {
		t.Skip("existing encrypted forensic response required")
	}
	cache, err := os.UserCacheDir()
	if err != nil || filepath.Clean(dir) != filepath.Join(cache, "OhMyCine-QuarkMultipartForensic-20261022") {
		t.Fatal("fixed_forensic_directory_required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := forensicReplayLocal(t, ctx, original, dir)
	if err != nil {
		t.Fatal("local_replay_rejected_no_network")
	}
	raw, _ := json.Marshal(out)
	t.Log(string(raw))
}
func TestQuarkMultipartForensicOfflineReplay(t *testing.T) {
	for _, tc := range []struct {
		name, body        string
		parse, put, guard bool
		metadata          int64
	}{
		{"valid", `{"status":0,"errno":0,"data":{"upload_urls":[{"part_number":2,"upload_url":"https://private.invalid/SECRET","signature_info":{"signature":"SECRET-SIGNATURE"}}],"common_headers":{"Content-Type":"application/octet-stream"}}}`, true, true, true, 0},
		{"header_type", `{"status":0,"data":{"upload_urls":[{"part_number":2,"upload_url":"https://private.invalid/SECRET","signature_info":{"signature":"SECRET"}}],"common_headers":{"Content-Type":["SECRET"]}}}`, false, false, true, 0},
		{"header_rejected", `{"status":0,"data":{"upload_urls":[{"part_number":2,"upload_url":"https://private.invalid/SECRET","signature_info":{"signature":"SECRET"}}],"common_headers":{"Cookie":"SECRET"}}}`, true, false, true, 0},
		{"wrong_part", `{"status":0,"data":{"upload_urls":[{"part_number":1,"upload_url":"https://private.invalid/SECRET","signature_info":{"signature":"SECRET"}}]}}`, false, false, false, 0},
		{"business", `{"status":-1,"errno":23018,"error_info":"SECRET","data":null}`, false, false, false, 0},
		{"unknown_length_budget", `{"status":0,"data":{"upload_urls":[{"part_number":2,"upload_url":"https://private.invalid/SECRET","signature_info":{"signature":"SECRET"}}]}}`, true, true, true, 8<<20 - 10000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original, dir := forensicOfflineOriginal(t, tc.metadata), observationOfflinePrivateDir(t)
			o, e := openForensicOriginal(original)
			if e != nil {
				t.Fatal("original")
			}
			s, e := openForensicStore(dir, o, false)
			if e != nil {
				t.Fatal("ledger")
			}
			s.state.Reserved = true
			s.state.ResponseSaved = true
			s.state.Outcome = "response"
			s.state.HTTPStatus = 200
			s.state.RequestBytes = int64(len(o.body))
			s.state.ResponseReserved = multipartForensicLimit + 1
			s.state.RawResponse = []byte(tc.body)
			s.state.ResponseBytes = int64(len(tc.body))
			if s.save() != nil {
				t.Fatal("seed encrypted response")
			}
			s.lock.Close()
			o.lock.Close()
			r, e := forensicReplayLocal(t, context.Background(), original, dir)
			if e != nil || !r.OriginalUnchanged || !r.ForensicUnchanged || r.NetworkRequests != 0 || r.Direct.NativeParserAccepted != tc.parse || r.Direct.NativePutPrepared != tc.put || r.GuardDestinationAccepted != tc.guard {
				t.Fatal("offline replay stage mismatch")
			}
			if tc.parse && tc.guard && r.Guarded != r.Direct {
				t.Fatal("guarded native disagrees")
			}
			if tc.metadata > 0 {
				if !r.ExactLengthReservationFits || r.UnknownLengthReservationFits || r.UnknownLengthGuarded.NativeParserAccepted || r.UnknownLengthDestinationAccepted || r.MetadataRemaining != 10000 {
					t.Fatal("unknown-length response must respect original remaining budget")
				}
			} else if r.UnknownLengthGuarded != r.Guarded || r.UnknownLengthDestinationAccepted != r.GuardDestinationAccepted {
				t.Fatal("sufficient budget must preserve unknown-length parser behavior")
			}
			raw, _ := json.Marshal(r)
			if bytes.Contains(raw, []byte("SECRET")) || bytes.Contains(raw, []byte("private.invalid")) {
				t.Fatal("private response escaped")
			}
		})
	}
}
