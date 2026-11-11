package pan115

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	pan115sdk "github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"golang.org/x/time/rate"
)

type uploadTestSDK struct {
	*bulkSDK
	pages        map[int64][]pan115sdk.File
	uploadCalls  int
	uploadParent string
	uploadName   string
	uploadSize   int64
	uploadBody   []byte
	uploadErr    error
	listErr      error
}

func (s *uploadTestSDK) UploadFastOrByMultipart(parentID, name string, size int64, file *os.File, _ ...pan115sdk.UploadMultipartOption) error {
	s.uploadCalls++
	s.uploadParent, s.uploadName, s.uploadSize = parentID, name, size
	body, err := io.ReadAll(file)
	if err != nil {
		return err
	}
	s.uploadBody = append([]byte(nil), body...)
	return s.uploadErr
}

func (s *uploadTestSDK) ListPage(_ string, offset, _ int64, _ ...pan115sdk.ListOption) (*[]pan115sdk.File, error) {
	s.listPageCalls++
	if s.listErr != nil {
		return nil, s.listErr
	}
	items := append([]pan115sdk.File(nil), s.pages[offset]...)
	return &items, nil
}

func newUploadTestClient(sdk sdkClient) *Client {
	return &Client{
		sdk: sdk, listRate: rate.NewLimiter(rate.Inf, 1), uploadRate: rate.NewLimiter(rate.Inf, 1),
		callSlots: make(chan struct{}, maxInFlightCalls), now: time.Now, jitter: func() time.Duration { return 0 },
	}
}

func managedUploadFile(t *testing.T, body []byte) *os.File {
	t.Helper()
	path := t.TempDir() + string(os.PathSeparator) + "upload.bin"
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func TestUploadUsesManagedFileAndReconcilesAcrossPages(t *testing.T) {
	body := []byte("managed upload")
	firstPage := make([]pan115sdk.File, maxPageSize)
	for index := range firstPage {
		firstPage[index] = pan115sdk.File{FileID: "filler", ParentID: "parent", Name: "other.bin", Size: 1}
	}
	sdk := &uploadTestSDK{bulkSDK: &bulkSDK{}, pages: map[int64][]pan115sdk.File{
		0:           firstPage,
		maxPageSize: {{FileID: "uploaded", ParentID: "parent", Name: "video.mkv", Size: int64(len(body))}},
	}}
	client := newUploadTestClient(sdk)
	if !client.Capabilities().FileUpload {
		t.Fatal("115 did not advertise file upload")
	}
	item, err := client.Upload(context.Background(), cloud.UploadRequest{ParentID: "parent", Name: "video.mkv", Size: int64(len(body)), Reader: managedUploadFile(t, body)})
	if err != nil {
		t.Fatal(err)
	}
	if item.ID != "uploaded" || item.ParentID != "parent" || item.Name != "video.mkv" || item.Size != int64(len(body)) {
		t.Fatalf("item=%+v", item)
	}
	if sdk.uploadCalls != 1 || sdk.uploadParent != "parent" || sdk.uploadName != "video.mkv" || sdk.uploadSize != int64(len(body)) || !bytes.Equal(sdk.uploadBody, body) || sdk.listPageCalls != 2 {
		t.Fatalf("sdk=%+v", sdk)
	}
}

func TestUploadRejectsNonFileReaderAndChangedFileBeforeProviderCall(t *testing.T) {
	sdk := &uploadTestSDK{bulkSDK: &bulkSDK{}, pages: map[int64][]pan115sdk.File{}}
	client := newUploadTestClient(sdk)
	if _, err := client.Upload(context.Background(), cloud.UploadRequest{ParentID: "parent", Name: "video.mkv", Size: 5, Reader: bytes.NewReader([]byte("video"))}); err == nil {
		t.Fatal("non-file reader was accepted")
	} else if code, retryable := cloud.ErrorInfo(err); code != cloud.CodeResponseInvalid || retryable {
		t.Fatalf("non-file code=%q retryable=%t err=%v", code, retryable, err)
	}
	if _, err := client.Upload(context.Background(), cloud.UploadRequest{ParentID: "parent", Name: "video.mkv", Size: 6, Reader: managedUploadFile(t, []byte("video"))}); err == nil {
		t.Fatal("changed file size was accepted")
	} else if code, retryable := cloud.ErrorInfo(err); code != cloud.CodeResponseInvalid || retryable {
		t.Fatalf("changed-file code=%q retryable=%t err=%v", code, retryable, err)
	}
	if sdk.uploadCalls != 0 {
		t.Fatalf("invalid request reached provider: %d", sdk.uploadCalls)
	}
}

func TestUploadFailsClosedWhenProviderResultIsMissingOrAmbiguous(t *testing.T) {
	body := []byte("managed upload")
	tests := []struct {
		name  string
		files []pan115sdk.File
	}{
		{name: "missing"},
		{name: "ambiguous", files: []pan115sdk.File{
			{FileID: "one", ParentID: "parent", Name: "video.mkv", Size: int64(len(body))},
			{FileID: "two", ParentID: "parent", Name: "video.mkv", Size: int64(len(body))},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sdk := &uploadTestSDK{bulkSDK: &bulkSDK{}, pages: map[int64][]pan115sdk.File{0: test.files}}
			_, err := newUploadTestClient(sdk).Upload(context.Background(), cloud.UploadRequest{ParentID: "parent", Name: "video.mkv", Size: int64(len(body)), Reader: managedUploadFile(t, body)})
			if err == nil {
				t.Fatal("ambiguous provider result was accepted")
			}
			if code, retryable := cloud.ErrorInfo(err); code != cloud.CodeMutationUnknown || !retryable {
				t.Fatalf("code=%q retryable=%t err=%v", code, retryable, err)
			}
			if sdk.uploadCalls != 1 {
				t.Fatalf("upload calls=%d", sdk.uploadCalls)
			}
		})
	}

}

func TestUploadMapsProviderFailureWithoutAttemptingReconciliation(t *testing.T) {
	body := []byte("managed upload")
	sdk := &uploadTestSDK{bulkSDK: &bulkSDK{}, pages: map[int64][]pan115sdk.File{}, uploadErr: errors.New("HTTP 429 rate limited")}
	_, err := newUploadTestClient(sdk).Upload(context.Background(), cloud.UploadRequest{ParentID: "parent", Name: "video.mkv", Size: int64(len(body)), Reader: managedUploadFile(t, body)})
	if code, retryable := cloud.ErrorInfo(err); code != cloud.CodeRateLimited || !retryable {
		t.Fatalf("code=%q retryable=%t err=%v", code, retryable, err)
	}
	if sdk.uploadCalls != 1 || sdk.listPageCalls != 0 {
		t.Fatalf("upload=%d list=%d", sdk.uploadCalls, sdk.listPageCalls)
	}
}

func TestUploadTreatsPostMutationReconciliationFailureAsUnknownOutcome(t *testing.T) {
	body := []byte("managed upload")
	sdk := &uploadTestSDK{bulkSDK: &bulkSDK{}, pages: map[int64][]pan115sdk.File{}, listErr: errors.New("HTTP 503 unavailable")}
	_, err := newUploadTestClient(sdk).Upload(context.Background(), cloud.UploadRequest{ParentID: "parent", Name: "video.mkv", Size: int64(len(body)), Reader: managedUploadFile(t, body)})
	if code, retryable := cloud.ErrorInfo(err); code != cloud.CodeMutationUnknown || !retryable {
		t.Fatalf("code=%q retryable=%t err=%v", code, retryable, err)
	}
	if sdk.uploadCalls != 1 || sdk.listPageCalls != 1 {
		t.Fatalf("upload=%d list=%d", sdk.uploadCalls, sdk.listPageCalls)
	}
}

var _ uploadSDK = (*uploadTestSDK)(nil)

// Exercise the OSS primitives over real loopback HTTP, including retransmission
// and cancellation. No provider credentials or public endpoint are involved.
func TestObservedUploadTransfersBodiesAndReconcilesCompletion(t *testing.T) {
	for _, mode := range []string{"retry", "callback_rejected", "cancel", "small_callback_rejected", "rapid"} {
		t.Run(mode, func(t *testing.T) {
			body := bytes.Repeat([]byte("managed upload body\n"), 14_000)
			if mode == "small_callback_rejected" {
				body = body[:512]
			}
			file := managedUploadFile(t, body)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var mu sync.Mutex
			attempts := map[int]int{}
			parts := map[int][]byte{}
			var uploaded, aborted, completed, requests int
			var samples []cloud.UploadProgress
			partStarted, partExited := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests++
				mu.Unlock()
				if r.Header.Get("x-oss-security-token") != "local-token" {
					t.Error("OSS token option was lost")
				}
				switch {
				case r.Method == http.MethodPost && r.URL.Query().Has("uploads"):
					w.Header().Set("Content-Type", "application/xml")
					_, _ = io.WriteString(w, `<InitiateMultipartUploadResult><Bucket>local-bucket</Bucket><Key>managed.bin</Key><UploadId>local-upload</UploadId></InitiateMultipartUploadResult>`)
				case r.Method == http.MethodPut:
					part, _ := strconv.Atoi(r.URL.Query().Get("partNumber"))
					if mode == "cancel" {
						close(partStarted)
						defer close(partExited)
						_, _ = io.Copy(io.Discard, r.Body)
						<-r.Context().Done()
						return
					}
					data, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					mu.Lock()
					attempts[part]++
					attempt := attempts[part]
					uploaded += len(data)
					parts[part] = data
					mu.Unlock()
					if mode == "retry" && part == 1 && attempt == 1 {
						w.WriteHeader(http.StatusBadRequest)
						_, _ = io.WriteString(w, `<Error><Code>OperationAborted</Code><Message>local retry</Message></Error>`)
						return
					}
					w.Header().Set("ETag", fmt.Sprintf("\"part-%d\"", part))
					if mode == "small_callback_rejected" {
						_, _ = io.WriteString(w, `{"state":false,"errno":99}`)
					}
				case r.Method == http.MethodPost:
					_, _ = io.Copy(io.Discard, r.Body)
					mu.Lock()
					completed++
					mu.Unlock()
					if r.Header.Get("x-oss-callback") == "" || r.Header.Get("x-oss-callback-var") == "" {
						t.Error("completion callback options were lost")
					}
					if mode == "callback_rejected" {
						_, _ = io.WriteString(w, `{"state":false,"errno":99}`)
					} else {
						_, _ = io.WriteString(w, `{"state":true}`)
					}
				case r.Method == http.MethodDelete:
					mu.Lock()
					aborted++
					mu.Unlock()
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected local OSS request: %s", r.Method)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer server.Close()
			initial := &pan115sdk.UploadInitResp{Status: 1}
			initial.Bucket, initial.Object = "local-bucket", "managed.bin"
			initial.Callback.Callback, initial.Callback.CallbackVar = "local callback", "local callback vars"
			if mode == "rapid" {
				initial.Status = 2
			}
			request := cloud.UploadRequest{ParentID: "parent", Name: "managed.bin", Size: int64(len(body)), Reader: file, Progress: func(sample cloud.UploadProgress) {
				mu.Lock()
				defer mu.Unlock()
				samples = append(samples, sample)
			}}
			tokenCalls := 0
			loadToken := func() (*pan115sdk.UploadOSSTokenResp, error) {
				tokenCalls++
				return &pan115sdk.UploadOSSTokenResp{AccessKeyID: "local-id", AccessKeySecret: "local-secret", SecurityToken: "local-token"}, nil
			}
			result := make(chan error, 1)
			go func() { result <- uploadOSSObserved(ctx, request, file, initial, server.URL, loadToken) }()
			if mode == "cancel" {
				select {
				case <-partStarted:
					cancel()
				case <-ctx.Done():
					t.Fatal("local part request did not start")
				}
			}
			var err error
			select {
			case err = <-result:
			case <-time.After(7 * time.Second):
				t.Fatal("upload did not join after cancellation/deadline")
			}
			if mode == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
				select {
				case <-partExited:
				case <-time.After(time.Second):
					t.Fatal("cancelled HTTP request remained active")
				}
			} else if mode == "retry" || mode == "rapid" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("failed completion callback was accepted")
			}
			if _, err := file.Stat(); err != nil {
				t.Fatalf("upload closed its caller-owned managed file: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if mode == "rapid" {
				if tokenCalls != 0 || requests != 0 || len(samples) != 0 {
					t.Fatalf("rapid upload invented transfer measurements: token=%d requests=%d samples=%d", tokenCalls, requests, len(samples))
				}
				return
			}
			if len(samples) == 0 {
				t.Fatal("HTTP body transfer produced no samples")
			}
			var previous cloud.UploadProgress
			for _, sample := range samples {
				if sample.Phase != "uploading_target" || sample.TotalBytes != int64(len(body)) || sample.TransferredBytes < previous.TransferredBytes || sample.TransferredBytes > sample.TotalBytes || sample.NetworkBytes <= previous.NetworkBytes || sample.SampledAt.IsZero() {
					t.Fatalf("invalid body sample: %+v after %+v", sample, previous)
				}
				previous = sample
			}
			if mode == "retry" {
				var actual []byte
				for part := 1; part <= len(parts); part++ {
					actual = append(actual, parts[part]...)
				}
				if !bytes.Equal(actual, body) || attempts[1] != 2 || len(parts) < 2 || completed != 1 || aborted != 0 || previous.TransferredBytes != int64(len(body)) || previous.NetworkBytes != int64(uploaded) || previous.NetworkBytes <= previous.TransferredBytes {
					t.Fatalf("multipart retry lost content or double-counted logical bytes: parts=%d attempts=%v completed=%d aborted=%d last=%+v received=%d", len(parts), attempts, completed, aborted, previous, uploaded)
				}
			} else if mode != "small_callback_rejected" && aborted != 1 {
				t.Fatalf("unfinished multipart upload not aborted once: %d", aborted)
			}
		})
	}
	t.Run("source_hashing_and_context_isolation", func(t *testing.T) {
		type attemptKey struct{}
		firstStarted, secondStarted, releaseSecond := make(chan struct{}), make(chan struct{}), make(chan struct{})
		httpClient := &http.Client{Timeout: 3 * time.Second, Transport: shareReadTransport(func(r *http.Request) (*http.Response, error) {
			if r.UserAgent() != pan115sdk.UA115Browser || r.Header.Get("X-Account-Scope") != "local-scope" {
				t.Error("upload lost configured headers")
			}
			if cookie, err := r.Cookie("UID"); err != nil || cookie.Value != "local-user" {
				t.Error("upload lost SDK credential cookie")
			}
			switch r.URL.Path {
			case "/first":
				if r.Context().Value(attemptKey{}) != "first" {
					t.Error("first upload adopted another request context")
				}
				close(firstStarted)
				<-r.Context().Done()
				return nil, r.Context().Err()
			case "/second":
				if r.Context().Value(attemptKey{}) != "second" {
					t.Error("second upload adopted another request context")
				}
				close(secondStarted)
				select {
				case <-releaseSecond:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewBufferString(`{"state":true,"cid":"123","data":[{"n":"tiny.bin","s":3,"sha":"ABC"}]}`)), Request: r}, nil
		})}
		sdk := pan115sdk.New(pan115sdk.WithClient(httpClient), pan115sdk.UA(pan115sdk.UA115Browser))
		sdk.Client.Header.Set("X-Account-Scope", "local-scope")
		sdk.SetCookies(&http.Cookie{Name: "UID", Value: "local-user"})
		sdk.UserID, sdk.Userkey, sdk.UploadMetaInfo = 42, "local-key", &pan115sdk.UploadMetaInfo{SizeLimit: 1024}
		adapter := &sdkAdapter{sdk}
		firstCtx, stopFirst := context.WithCancel(context.WithValue(context.Background(), attemptKey{}, "first"))
		defer stopFirst()
		secondCtx, stopSecond := context.WithTimeout(context.WithValue(context.Background(), attemptKey{}, "second"), 2*time.Second)
		defer stopSecond()
		first, second := adapter.uploadClient(firstCtx), adapter.uploadClient(secondCtx)
		if first.Client.GetClient() != httpClient || second.Client.GetClient() != httpClient || first.UserID != sdk.UserID || first.Userkey != sdk.Userkey || first.UploadMetaInfo != sdk.UploadMetaInfo || first.Client.RetryCount != sdk.Client.RetryCount {
			t.Fatal("upload changed configured SDK identity or HTTP policy")
		}
		firstDone, secondDone := make(chan error, 1), make(chan error, 1)
		go func() { _, err := first.NewRequest().Get("http://upload.local.test/first"); firstDone <- err }()
		select {
		case <-firstStarted:
		case <-secondCtx.Done():
			t.Fatal("first local preparation did not start")
		}
		go func() { _, err := second.NewRequest().Get("http://upload.local.test/second"); secondDone <- err }()
		select {
		case <-secondStarted:
		case <-secondCtx.Done():
			t.Fatal("second local preparation did not start")
		}
		stopFirst()
		if err := <-firstDone; !errors.Is(err, context.Canceled) {
			t.Fatalf("preparation cancellation lost: %v", err)
		}
		close(releaseSecond)
		if err := <-secondDone; err != nil {
			t.Fatalf("another upload cancellation leaked: %v", err)
		}
		first.Client.Header.Set("X-Account-Scope", "changed")
		if sdk.Client.Header.Get("X-Account-Scope") != "local-scope" || second.Client.Header.Get("X-Account-Scope") != "local-scope" {
			t.Fatal("request-local headers changed a shared SDK")
		}
		request := cloud.UploadRequest{ParentID: "123", Name: "tiny.bin", Size: 3}
		if err := verifySmallUpload(second, request, "ABC"); err != nil {
			t.Fatal(err)
		}
		if err := verifySmallUpload(second, request, "other digest"); err == nil {
			t.Fatal("small upload accepted a different target digest")
		}
		var hashing []cloud.UploadProgress
		request.Size, request.Reader = 4, managedUploadFile(t, []byte("abc"))
		request.Progress = func(sample cloud.UploadProgress) { hashing = append(hashing, sample) }
		if err := adapter.UploadObserved(context.Background(), request, request.Reader.(*os.File)); err == nil {
			t.Fatal("hashing did not detect changed source size")
		}
		if len(hashing) != 1 || hashing[0].Phase != "hashing_source" || hashing[0].TransferredBytes != 0 || hashing[0].NetworkBytes != 0 || !hashing[0].SampledAt.IsZero() {
			t.Fatalf("hashing invented upload byte samples: %+v", hashing)
		}
	})
}
