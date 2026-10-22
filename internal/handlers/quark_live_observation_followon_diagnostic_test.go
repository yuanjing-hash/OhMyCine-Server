//go:build quark_live

package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

// This loader has no key/journal creation or save path. Expired batches remain
// inspectable, but the transport's original deadline is never extended.
func openExistingFollowDiagnosticStore(dir string, parent *followParent) (*followStore, error) {
	if !filepath.IsAbs(dir) || observationCheckPrivateDirectory(dir) != nil {
		return nil, observationError()
	}
	lock, err := database.AcquireExclusiveRuntime(filepath.Join(dir, "followon"))
	if err != nil {
		return nil, observationError()
	}
	ok := false
	defer func() {
		if !ok {
			lock.Close()
		}
	}()
	journal, err := liveTVRead(filepath.Join(dir, "followon.enc"), 2<<20, false)
	if err != nil || len(journal) == 0 {
		return nil, observationError()
	}
	key, err := liveTVRead(filepath.Join(dir, "followon.key"), 1024, false)
	if err != nil || len(key) == 0 {
		return nil, observationError()
	}
	cipher, err := credential.Open("", string(key))
	if err != nil {
		return nil, observationError()
	}
	plain, err := cipher.Decrypt(followPurpose, string(journal))
	s := &followStore{dir: dir, lock: lock, cipher: cipher}
	if err != nil || liveTVDecode([]byte(plain), &s.state) != nil || !followManifestValid(s.state) || !reflect.DeepEqual(s.state.Identity, parent.identity) {
		return nil, observationError()
	}
	ok = true
	return s, nil
}

func TestQuarkObservationFollowonDiagnosticExpiredReadOnly(t *testing.T) {
	p, s := followIndependentStore(t)
	_, _, key := followIndependentRequest(t, s, "rename")
	s.state.Writes[key] = true
	r := s.state.Operations[s.state.RenameID]
	r.State = "unknown"
	s.state.Operations[r.ID] = r
	s.state.Started = time.Now().Add(-time.Hour)
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	journal, _ := os.ReadFile(filepath.Join(s.dir, "followon.enc"))
	cipherKey, _ := os.ReadFile(filepath.Join(s.dir, "followon.key"))
	s.lock.Close()
	loaded, err := openExistingFollowDiagnosticStore(s.dir, p)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.lock.Close()
	report, err := followDiagnostic(loaded)
	if err != nil || !report.DeadlineExpired || report.ReceiptState != "unknown" || report.OriginalHTTPRecorded || report.OriginalErrnoRecorded {
		t.Fatalf("safe diagnostic contract: %+v", report)
	}
	if followQueryRenameDiagnostic(t, loaded) == nil {
		t.Fatal("expired network query allowed")
	}
	after, _ := os.ReadFile(filepath.Join(s.dir, "followon.enc"))
	afterKey, _ := os.ReadFile(filepath.Join(s.dir, "followon.key"))
	if string(journal) != string(after) || string(cipherKey) != string(afterKey) {
		t.Fatal("diagnostic changed frozen state")
	}
}

type followDiagnosticTransportFunc func(*http.Request) (*http.Response, error)

func (f followDiagnosticTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestQuarkObservationFollowonDiagnosticRejectsWrites(t *testing.T) {
	for _, kind := range []string{"rename", "mkdir", "move"} {
		t.Run(kind, func(t *testing.T) {
			_, s := followIndependentStore(t)
			path, body, _ := followIndependentRequest(t, s, kind)
			g := newFollowGuard(s)
			g.delay = 0
			g.renameQueryOnly = true
			calls := 0
			base := followDiagnosticTransportFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unreachable") })
			_, err := g.wrap(base, false).RoundTrip(followIndependentHTTP(path, body))
			if err == nil || calls != 0 || s.count() != 0 {
				t.Fatal("read-only diagnostic dispatched a mutation")
			}
		})
	}
}

func TestQuarkObservationFollowonDiagnosticNativeOriginalQuery(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "still_old_name", true: "confirmed_new_name"}[changed], func(t *testing.T) {
			_, s := followIndependentStore(t)
			_, _, writeKey := followIndependentRequest(t, s, "rename")
			r := s.state.Operations[s.state.RenameID]
			r.State = "unknown"
			s.state.Operations[r.ID] = r
			s.state.Writes[writeKey] = true
			if err := s.save(); err != nil {
				t.Fatal(err)
			}
			i := s.state.Identity
			name := i.Sample.Name
			if changed {
				name = s.state.RenameName
			}
			info := func(id, parent, name string, dir bool, size int64) map[string]any {
				kind := "1"
				if dir {
					kind = "0"
				}
				return map[string]any{"fid": id, "parent_fid": parent, "filename": name, "file_type": kind, "size": size}
			}
			g := newFollowGuard(s)
			g.delay = 0
			g.startup = false
			g.renameQueryOnly = true
			calls := 0
			fixture := followDiagnosticTransportFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				var data any
				switch req.URL.Path {
				case "/open/v1/file/info":
					switch req.URL.Query().Get("fid") {
					case i.SampleAgent:
						data = info(i.SampleAgent, i.RootAgent, name, false, observationFixtureSize)
					case i.RootAgent:
						data = info(i.RootAgent, i.ProofAgent, i.Root.Name, true, 0)
					case i.ProofAgent:
						data = info(i.ProofAgent, "0", "proof", true, 0)
					default:
						t.Fatal("unexpected identity")
					}
				case "/open/v1/file/list":
					data = map[string]any{"file_list": []any{info(i.SampleAgent, i.RootAgent, name, false, observationFixtureSize)}, "last_page": true}
				default:
					t.Fatal("unexpected cloud write or other endpoint")
				}
				body, _ := json.Marshal(map[string]any{"status": 0, "data": data})
				return observationOfflineResponse(string(body)), nil
			})
			raw, _ := s.credentials(context.Background())
			d, err := quark.NewObservationValidationClient(cloud.Config{Credentials: raw, Operations: s}, func(_ http.RoundTripper, media bool) http.RoundTripper { return g.wrap(fixture, media) })
			if err != nil {
				t.Fatal(err)
			}
			result, err := d.QueryOperation(context.Background(), s.state.RenameID)
			if changed {
				if err != nil || !result.Complete || s.state.Operations[r.ID].State != "confirmed" {
					t.Fatalf("read-only confirmation failed: %v calls=%d endpoint=%s", err, calls, g.lastEndpoint)
				}
			} else {
				var failure *quark.Failure
				if !errors.As(err, &failure) || failure.Code != cloud.CodeMutationUnknown || s.state.Operations[r.ID].State != "unknown" {
					t.Fatalf("old name was treated as rejected or successful: %v calls=%d endpoint=%s", err, calls, g.lastEndpoint)
				}
			}
			if calls < 1 || calls > 4 || len(s.state.Writes) != 1 || !s.state.Writes[writeKey] {
				t.Fatal("query escaped original identity/budget")
			}
		})
	}
}

func TestQuarkObservationFollowonDiagnosticRejectsOtherTask(t *testing.T) {
	_, s := followIndependentStore(t)
	_, _, _ = followIndependentRequest(t, s, "mkdir")
	r := s.state.Operations[s.state.DirectoryID]
	r.State = "accepted"
	var p followOperation
	_ = json.Unmarshal(r.Payload, &p)
	p.TaskID = "private-other-task"
	r.Payload, _ = json.Marshal(p)
	s.state.Operations[r.ID] = r
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	g := newFollowGuard(s)
	g.delay = 0
	g.renameQueryOnly = true
	calls := 0
	base := followDiagnosticTransportFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unreachable") })
	_, err := g.wrap(base, false).RoundTrip(followIndependentHTTP("/open/v1/task/query", map[string]any{"task_id": p.TaskID}))
	if err == nil || calls != 0 || s.count() != 0 {
		t.Fatal("diagnostic queried another operation")
	}
}

func TestQuarkObservationFollowonDiagnosticRecordedFailure(t *testing.T) {
	_, s := followIndependentStore(t)
	_, _, _ = followIndependentRequest(t, s, "rename")
	r := s.state.Operations[s.state.RenameID]
	r.State = "unknown"
	var body map[string]json.RawMessage
	_ = json.Unmarshal(r.Payload, &body)
	body["submit_failure"] = json.RawMessage(`{"http_status":400,"errno":1234}`)
	r.Payload, _ = json.Marshal(body)
	s.state.Operations[r.ID] = r
	report, err := followDiagnostic(s)
	if err != nil || !report.OriginalHTTPRecorded || !report.OriginalErrnoRecorded || report.OriginalHTTP == nil || *report.OriginalHTTP != 400 || report.OriginalErrno == nil || *report.OriginalErrno != 1234 {
		t.Fatal("new numeric receipt evidence incorrectly discarded")
	}
	body["submit_failure"] = json.RawMessage(`{"http_status":400}`)
	r.Payload, _ = json.Marshal(body)
	s.state.Operations[r.ID] = r
	report, err = followDiagnostic(s)
	if err != nil || !report.OriginalHTTPRecorded || report.OriginalErrnoRecorded || report.OriginalErrno != nil {
		t.Fatal("missing errno incorrectly treated as recorded zero")
	}
}

type followDiagnosticReport struct {
	ReceiptState          string `json:"rename_receipt_state"`
	Dispatched            bool   `json:"rename_dispatched"`
	HasAcknowledgement    bool   `json:"has_acknowledgement"`
	HasTaskID             bool   `json:"has_task_id"`
	OriginalHTTPRecorded  bool   `json:"original_http_recorded"`
	OriginalErrnoRecorded bool   `json:"original_errno_recorded"`
	OriginalHTTP          *int   `json:"original_http,omitempty"`
	OriginalErrno         *int   `json:"original_errno,omitempty"`
	DeadlineExpired       bool   `json:"deadline_expired"`
	BudgetExhausted       bool   `json:"budget_exhausted"`
	Completed             bool   `json:"completed"`
	Requests              int    `json:"requests"`
	API                   int    `json:"api"`
}

func followDiagnostic(s *followStore) (followDiagnosticReport, error) {
	st := s.state
	r := followDiagnosticReport{ReceiptState: "not_created", Dispatched: st.Writes[st.RenameID+":rename"], DeadlineExpired: time.Since(st.Started) >= 12*time.Minute, BudgetExhausted: st.Requests >= 220 || st.API >= 200, Completed: st.Completed, Requests: st.Requests, API: st.API}
	receipt, exists := st.Operations[st.RenameID]
	if !exists {
		if r.Dispatched {
			return r, observationError()
		}
		return r, nil
	}
	var p followOperation
	if !s.allowed(receipt) || receipt.Kind != "rename" || json.Unmarshal(receipt.Payload, &p) != nil || p.Request.ID != st.RenameID || p.Request.Kind != "rename" || p.Request.Name != st.RenameName {
		return r, observationError()
	}
	switch receipt.State {
	case "prepared", "submitting", "unknown", "accepted", "confirmed", "failed":
		r.ReceiptState = receipt.State
	default:
		return r, observationError()
	}
	r.HasAcknowledgement = p.Handle != ""
	r.HasTaskID = p.TaskID != ""
	if p.SubmitFailure != nil && p.SubmitFailure.HTTPStatus >= 100 && p.SubmitFailure.HTTPStatus <= 599 {
		r.OriginalHTTPRecorded = true
		httpStatus := p.SubmitFailure.HTTPStatus
		r.OriginalHTTP = &httpStatus
		if p.SubmitFailure.Errno != nil {
			r.OriginalErrnoRecorded = true
			errno := *p.SubmitFailure.Errno
			r.OriginalErrno = &errno
		}
	}
	// Older receipts did not persist HTTP/errno. Missing is not zero or a
	// definitive rejection, even when an operator separately saw HTTP 400.
	return r, nil
}

func followQueryRenameDiagnostic(t *testing.T, s *followStore) error {
	t.Helper()
	report, err := followDiagnostic(s)
	if err != nil || !report.Dispatched || report.Completed || report.DeadlineExpired || report.BudgetExhausted || report.ReceiptState == "not_created" {
		return observationError()
	}
	ctx, cancel := context.WithDeadline(context.Background(), s.state.Started.Add(12*time.Minute))
	defer cancel()
	g := newFollowGuard(s)
	g.startup = false
	g.renameQueryOnly = true
	raw, err := s.credentials(ctx)
	if err != nil {
		return err
	}
	d, err := quark.NewObservationValidationClient(cloud.Config{Credentials: raw, Operations: s}, g.wrap)
	if err != nil {
		return err
	}
	before := s.count()
	result, queryErr := d.QueryOperation(ctx, s.state.RenameID)
	code := "ok"
	status := 0
	var errno *int
	if queryErr != nil {
		code = "query_failed"
		var failure *quark.Failure
		if errors.As(queryErr, &failure) {
			switch failure.Code {
			case cloud.CodeMutationUnknown:
				code = "mutation_unknown"
			case quark.CodeAuth:
				code = "authorization_unavailable"
			case quark.CodePending:
				code = "pending"
			case quark.CodeIdentity:
				code = "identity_mismatch"
			case quark.CodeInvalid:
				code = "invalid_response"
			case quark.CodeUnavailable:
				code = "upstream_unavailable"
			}
			if failure.HTTPStatus >= 100 && failure.HTTPStatus <= 599 {
				status = failure.HTTPStatus
				n := failure.Errno
				errno = &n
			}
		}
	}
	out, _ := json.Marshal(struct {
		Code      string `json:"code"`
		HTTP      int    `json:"http"`
		Errno     *int   `json:"errno"`
		Confirmed bool   `json:"confirmed"`
		Requests  int    `json:"requests"`
	}{code, status, errno, queryErr == nil && result.Complete && result.Receipt.State == "confirmed", s.count() - before})
	t.Log("original_rename_readonly_query=" + string(out))
	// A successful read which still sees the old name is inconclusive. No
	// mutation route, later service stage, or alternate operation is invoked.
	return nil
}
