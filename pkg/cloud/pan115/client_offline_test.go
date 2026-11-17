package pan115

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	pan115sdk "github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"golang.org/x/time/rate"
)

const testOfflineHash = "0123456789abcdef0123456789abcdef01234567"

type offlineRecoverySDK struct {
	*bulkSDK
	addHashes []string
	addErr    error
	tasks     []*pan115sdk.OfflineTask
	pageTasks map[int64][]*pan115sdk.OfflineTask
	pageCount int64
	addCalls  int
	listCalls int
}

func (s *offlineRecoverySDK) AddOfflineTaskURIs([]string, string, ...pan115sdk.OfflineOption) ([]string, error) {
	s.addCalls++
	return append([]string(nil), s.addHashes...), s.addErr
}

func (s *offlineRecoverySDK) ListOfflineTask(page int64) (pan115sdk.OfflineTaskResp, error) {
	s.listCalls++
	tasks := s.tasks
	if s.pageTasks != nil {
		tasks = s.pageTasks[page]
	}
	pageCount := s.pageCount
	if pageCount < 1 {
		pageCount = 1
	}
	return pan115sdk.OfflineTaskResp{Page: page, PageCount: pageCount, Tasks: tasks}, nil
}

func newOfflineTestClient(sdk sdkClient) *Client {
	return &Client{
		sdk:         sdk,
		offlineRate: rate.NewLimiter(rate.Inf, 1),
		callSlots:   make(chan struct{}, maxInFlightCalls),
		now:         time.Now,
		jitter:      func() time.Duration { return 0 },
	}
}

func TestNewUses115BrowserUserAgentForCookieAPIs(t *testing.T) {
	driver, err := New(cloud.Config{Cookie: "UID=1; CID=cid; SEID=seid"})
	if err != nil {
		t.Fatal(err)
	}
	client := driver.(*Client)
	adapter := client.sdk.(*sdkAdapter)
	if got := adapter.Client.Header.Get("User-Agent"); got != pan115sdk.UA115Browser {
		t.Fatalf("User-Agent = %q, want %q", got, pan115sdk.UA115Browser)
	}
	if adapter.Client.GetClient().Timeout != 15*time.Second || adapter.Client.RetryCount != 0 {
		t.Fatal("ordinary SDK writer lost its finite timeout or zero-retry contract")
	}
	for _, scenario := range []struct {
		name, body, code, kind    string
		status, errno             int
		definite, risk, transient bool
	}{
		{"http", "private-response-secret", cloud.CodeRateLimited, "http_error", 429, 0, false, true, false},
		{"transient-http", "private-response-secret", cloud.CodeUnavailable, "http_error", 503, 0, false, false, true},
		{"body-timeout", "", cloud.CodeUnavailable, "read_error", 200, 0, false, false, true},
		{"non-json", "private-response-secret", cloud.CodeUnavailable, "invalid_response", 200, 0, false, false, false},
		{"unknown-errno", `{"state":false,"errno":12345,"error":"private-response-secret"}`, cloud.CodeUnavailable, "provider_error", 200, 12345, false, false, false},
		{"provider-risk", `{"state":false,"errno":12345,"error":"请求频繁 private-response-secret"}`, cloud.CodeRateLimited, "provider_error", 200, 12345, false, true, false},
		{"typed-refusal", `{"state":false,"errno":"10004","error":"private-response-secret"}`, cloud.CodeOfflineBadLink, "provider_error", 200, 10004, true, false, false},
		{"bounded", strings.Repeat("x", (2<<20)+1), cloud.CodeUnavailable, "response_too_large", 200, 0, false, false, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			calls := 0
			body := &offlineTestBody{Reader: strings.NewReader(scenario.body)}
			if scenario.name == "body-timeout" {
				body.Reader = iotest.ErrReader(os.ErrDeadlineExceeded)
			}
			adapter.UserID = 1
			adapter.Client.GetClient().Transport = offlineDiagnosticTransport{base: selectedRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if ordinaryWireOperation(r) != "submit" || r.Header.Get("User-Agent") != pan115sdk.UA115Browser || r.Method != http.MethodPost {
					t.Fatal("ordinary SDK request route changed")
				}
				if err := r.ParseForm(); err != nil || len(r.PostForm) != 1 || r.PostForm.Get("data") == "" || strings.Contains(r.PostForm.Get("data"), "magnet:") {
					t.Fatal("ordinary SDK request is no longer the encrypted form")
				}
				return &http.Response{StatusCode: scenario.status, Header: http.Header{}, Body: body, Request: r}, nil
			})}
			_, wireErr := adapter.AddOfflineTaskURIs([]string{"magnet:?xt=urn:btih:" + testOfflineHash}, "root")
			err := downloader.WithFailureStage(mapError(wireErr), downloader.FailureOfflineSubmission)
			diagnostic, ok := downloader.FailureDiagnosticFromError(err)
			code, _ := cloud.ErrorInfo(err)
			if !ok || calls != 1 || !body.closed || code != scenario.code || diagnostic.ProviderOperation != "submit" || diagnostic.ProviderHTTPStatus != scenario.status || diagnostic.ProviderErrno != scenario.errno || diagnostic.ProviderResponseKind != scenario.kind || definiteOfflineRejection(wireErr) != scenario.definite {
				t.Fatalf("bounded ordinary response diagnostic mismatch: code=%s diagnostic=%+v", code, diagnostic)
			}
			if strings.Contains(err.Error()+wireErr.Error(), "private-response-secret") {
				t.Fatal("raw response escaped the native boundary")
			}
			if isRiskResponse(wireErr) != scenario.risk || isTransientResponse(wireErr) != scenario.transient {
				t.Fatal("safe response projection changed account recovery classification")
			}
			if diagnostic.Timeout != (scenario.name == "body-timeout") {
				t.Fatal("response wrapper lost the observed timeout property")
			}
		})
	}
	// A successful list body still reaches the unchanged SDK parser. Its
	// transport facts belong to this request, never a shared last-response slot.
	adapter.Client.GetClient().Transport = offlineDiagnosticTransport{base: selectedRoundTrip(func(r *http.Request) (*http.Response, error) {
		if ordinaryWireOperation(r) != "reconcile" {
			t.Fatal("wrong reconciliation request")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"state":true,"page":1,"page_count":1,"tasks":[]}`)), Request: r}, nil
	})}
	if page, err := adapter.ListOfflineTask(1); err != nil || page.Page != 1 || page.PageCount != 1 {
		t.Fatal("successful SDK response was not restored", err)
	}
	for _, kind := range []string{"ordinary", "wanted"} {
		t.Run("native-clock-after-identity-"+kind, func(t *testing.T) {
			adapter.UserID = 0
			reads, writes, dispatches := 0, 0, 0
			adapter.Client.GetClient().Transport = selectedRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "my.115.com" {
					reads++
					if dispatches != 0 {
						t.Fatal("identity preflight started the cloud clock")
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"state":true,"data":{"user_id":1}}`)), Request: r}, nil
				}
				writes++
				if dispatches != 1 || reads != 1 {
					t.Fatal("native write lacks original dispatch boundary")
				}
				return &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"state":false,"errno":10004}`)), Request: r}, nil
			})
			ctx := cloud.WithOfflineDispatchRecorder(context.Background(), func(time.Time) error {
				dispatches++
				if reads != 1 || writes != 0 {
					t.Fatal("clock does not name native Add boundary")
				}
				return nil
			})
			if kind == "ordinary" {
				_, _ = adapter.AddOfflineTaskURIsContext(ctx, []string{"https://example.test/file"}, "root")
			} else {
				_ = adapter.AddOfflineWanted(ctx, selectedWantedRequest{InfoHash: testOfflineHash, DirectoryID: "root", SavePath: "Pack", Wanted: []int{0}})
			}
			if reads != 1 || writes != 1 || dispatches != 1 {
				t.Fatal("native dispatch fixture did not cover identity plus write")
			}
		})
	}

}

type offlineTestBody struct {
	io.Reader
	closed bool
}

func (b *offlineTestBody) Close() error { b.closed = true; return nil }

func TestSubmitOfflineUsesReturnedIdentityWithoutRecovery(t *testing.T) {
	sdk := &offlineRecoverySDK{bulkSDK: &bulkSDK{}, addHashes: []string{testOfflineHash}}
	task, err := newOfflineTestClient(sdk).SubmitOffline(context.Background(), "magnet:?xt=urn:btih:"+testOfflineHash, "root")
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != testOfflineHash || sdk.addCalls != 1 || sdk.listCalls != 0 {
		t.Fatalf("task=%+v add=%d list=%d", task, sdk.addCalls, sdk.listCalls)
	}
}

func TestSubmitOfflineRecoversExistingMagnetAfterAmbiguousAddFailure(t *testing.T) {
	sdk := &offlineRecoverySDK{
		bulkSDK: &bulkSDK{},
		addErr:  errors.New("upstream response could not be decoded"),
		tasks:   []*pan115sdk.OfflineTask{{InfoHash: testOfflineHash, Status: 2, DelFileId: "output"}},
	}
	task, err := newOfflineTestClient(sdk).SubmitOffline(context.Background(), "magnet:?XT=URN:BTIH:"+testOfflineHash, "root")
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != testOfflineHash || !task.Completed || task.OutputItemID != "output" || sdk.addCalls != 1 || sdk.listCalls != 1 {
		t.Fatalf("task=%+v add=%d list=%d", task, sdk.addCalls, sdk.listCalls)
	}
}

func TestSubmitOfflineRecoversProviderDuplicateBeyondLegacyFivePageWindow(t *testing.T) {
	sdk := &offlineRecoverySDK{
		bulkSDK:   &bulkSDK{},
		addErr:    pan115sdk.ErrOfflineTaskExisted,
		pageCount: 8,
		pageTasks: map[int64][]*pan115sdk.OfflineTask{8: {{InfoHash: testOfflineHash, Status: 0}}},
	}
	task, err := newOfflineTestClient(sdk).SubmitOffline(context.Background(), "magnet:?xt=urn:btih:"+testOfflineHash, "root")
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != testOfflineHash || task.Status != "queued" || sdk.addCalls != 1 || sdk.listCalls != 8 {
		t.Fatalf("task=%+v add=%d list=%d", task, sdk.addCalls, sdk.listCalls)
	}
}

func TestSubmitOfflineReturnsStableConflictWhenDuplicateCannotBeAdopted(t *testing.T) {
	sdk := &offlineRecoverySDK{bulkSDK: &bulkSDK{}, addErr: pan115sdk.ErrOfflineTaskExisted}
	_, err := newOfflineTestClient(sdk).SubmitOffline(context.Background(), "magnet:?xt=urn:btih:"+testOfflineHash, "root")
	if code, retryable := cloud.ErrorInfo(err); code != cloud.CodeOfflineTaskExists || retryable {
		t.Fatalf("error code=%q retryable=%t err=%v", code, retryable, err)
	}
	if sdk.addCalls != 1 || sdk.listCalls != 1 {
		t.Fatalf("add=%d list=%d", sdk.addCalls, sdk.listCalls)
	}
}

func TestGetOfflineFallsBackWhenCachedTaskPageHasShifted(t *testing.T) {
	sdk := &offlineRecoverySDK{
		bulkSDK:   &bulkSDK{},
		pageCount: 3,
		pageTasks: map[int64][]*pan115sdk.OfflineTask{3: {{InfoHash: testOfflineHash, Status: 2, DelFileId: "output"}}},
	}
	client := newOfflineTestClient(sdk)
	client.offlinePages.Store(testOfflineHash, int64(2))
	task, err := client.GetOffline(context.Background(), testOfflineHash)
	if err != nil {
		t.Fatal(err)
	}
	if !task.Completed || task.OutputItemID != "output" || sdk.listCalls != 3 {
		t.Fatalf("task=%+v list=%d", task, sdk.listCalls)
	}
}

func TestSubmitOfflinePreservesFailureWhenNoExistingTaskCanBeRecovered(t *testing.T) {
	sdk := &offlineRecoverySDK{bulkSDK: &bulkSDK{}, addErr: errors.New("offline unavailable")}
	_, err := newOfflineTestClient(sdk).SubmitOffline(context.Background(), "https://example.test/file", "root")
	if code, retryable := cloud.ErrorInfo(err); code != cloud.CodeUnavailable || !retryable {
		t.Fatalf("error code=%q retryable=%t err=%v", code, retryable, err)
	}
	if sdk.listCalls != 0 {
		t.Fatalf("non-magnet recovery listed %d pages", sdk.listCalls)
	}
}

func TestOfflineInfoHashAcceptsHexAndBase32(t *testing.T) {
	for _, uri := range []string{
		"magnet:?xt=urn:btih:" + testOfflineHash,
		"MAGNET:?XT=urn:btih:AERUKZ4JVPG66AJDIVTYTK6N54ASGRLH",
	} {
		if got, ok := offlineInfoHash(uri); !ok || got != testOfflineHash {
			t.Fatalf("offlineInfoHash(%q) = %q, %t", uri, got, ok)
		}
	}
	for _, uri := range []string{"https://example.test/file", "magnet:?dn=missing", "magnet:?xt=urn:btih:not-a-hash"} {
		if got, ok := offlineInfoHash(uri); ok || got != "" {
			t.Fatalf("offlineInfoHash(%q) = %q, %t", uri, got, ok)
		}
	}
}

func TestMapOfflineTaskUsesCurrentProviderStatusSemantics(t *testing.T) {
	tests := []struct {
		providerStatus int
		status         string
		completed      bool
		failed         bool
	}{
		{providerStatus: 0, status: "queued"},
		{providerStatus: 1, status: "downloading"},
		{providerStatus: 2, status: "completed", completed: true},
		{providerStatus: 3, status: "queued"},
		{providerStatus: -1, status: "failed", failed: true},
	}
	for _, item := range tests {
		task := mapOfflineTask(&pan115sdk.OfflineTask{InfoHash: testOfflineHash, Status: item.providerStatus})
		if task.Status != item.status || task.Completed != item.completed || task.Failed != item.failed || task.ProviderStatus != item.providerStatus {
			t.Fatalf("provider status %d mapped to %+v", item.providerStatus, task)
		}
	}
}

func TestMapOfflineSubmissionErrorsKeepsPermanentFailuresOutOfRetryQueue(t *testing.T) {
	tests := []struct {
		err  error
		code string
	}{
		{err: pan115sdk.ErrOfflineNoTimes, code: cloud.CodeOfflineNoQuota},
		{err: pan115sdk.ErrOfflineInvalidLink, code: cloud.CodeOfflineBadLink},
	}
	for _, item := range tests {
		code, retryable := cloud.ErrorInfo(mapError(item.err))
		if code != item.code || retryable {
			t.Fatalf("mapError(%v) = %q, %t", item.err, code, retryable)
		}
	}
	for _, durable := range []bool{false, true} {
		sdk := &offlineRecoverySDK{bulkSDK: &bulkSDK{}, addErr: pan115sdk.ErrOfflineNoTimes}
		client := newOfflineTestClient(sdk)
		recorded := 0
		ctx := context.Background()
		if durable {
			ctx = cloud.WithOfflineNonAcceptanceRecorder(ctx, func() error { recorded++; return nil })
		}
		_, err := client.SubmitOffline(ctx, "https://example.test/source.bin", "root")
		if downloader.DefiniteSubmissionNonAcceptance(err) != durable || sdk.addCalls != 1 || recorded != map[bool]int{false: 0, true: 1}[durable] {
			t.Fatal("non-hash submission inferred non-acceptance without durable original proof", err)
		}
	}

}

func TestAppFileFinalizationRequiresExplicitSchemaAndDoesNotInvalidateWebCookie(t *testing.T) {
	for _, test := range []struct {
		name, fields string
		want         cloud.OfflineFinalization
		valid        bool
	}{
		{"complete", `"fta":"1"`, cloud.OfflineFinalizationComplete, true}, {"pending0", `"fta":0`, cloud.OfflineFinalizationPending, true}, {"pending2", `"fta":"2"`, cloud.OfflineFinalizationPending, true}, {"missing", `"sta":1`, cloud.OfflineFinalizationUnknown, true}, {"null", `"fta":null`, cloud.OfflineFinalizationUnknown, true}, {"unknown", `"fta":3`, cloud.OfflineFinalizationUnknown, true}, {"boolean", `"fta":true`, cloud.OfflineFinalizationUnknown, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := []byte(`{"state":true,"count":1,"data":[{"fid":"leaf","pid":"root","fn":"movie.mkv","fc":"1","fs":"10","aid":"1",` + test.fields + `}]}`)
			rows, _, valid := parseFinalizedAppPage(raw, "root")
			if valid != test.valid || len(rows) != 1 || rows[0].State != test.want {
				t.Fatalf("schema proof=%v rows=%+v", valid, rows)
			}
		})
	}
	for _, raw := range []string{`{"state":true,"count":1,"data":[{"fid":"leaf","pid":"other","fn":"a","fc":"1","fs":10,"aid":1,"fta":1}]}`, `{"state":true,"data":[]}`, `{"state":false,"count":0,"data":[]}`} {
		if _, _, ok := parseFinalizedAppPage([]byte(raw), "root"); ok {
			t.Fatal("unproven App response accepted")
		}
	}
	driver, err := New(cloud.Config{Cookie: "UID=1; CID=cid; SEID=seid"})
	if err != nil {
		t.Fatal(err)
	}
	client := driver.(*Client)
	client.listRate = rate.NewLimiter(rate.Inf, 1)
	adapter := client.sdk.(*sdkAdapter)
	for _, status := range []int{http.StatusUnauthorized, http.StatusOK} {
		adapter.Client.GetClient().Transport = selectedRoundTrip(func(r *http.Request) (*http.Response, error) {
			if r.URL.String() == "" || r.URL.Host != "proapi.115.com" || r.URL.Path != "/android/2.0/ufile/files" || r.URL.Query().Get("cur") != "1" || r.URL.Query().Get("cid") != "root" {
				t.Fatal("wrong App endpoint/schema")
			}
			return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"state":false,"errno":99}`)), Request: r}, nil
		})
		result, err := client.FinalizedFiles(context.Background(), []cloud.Item{{ID: "leaf", ParentID: "root", Name: "movie.mkv", Size: 10}})
		if err != nil || len(result) != 0 || client.authExpired {
			t.Fatal("optional App proof failure poisoned usable offline Cookie", err)
		}
	}
}
