package quark

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

type failedObjectBody struct{}

func (failedObjectBody) Read([]byte) (int, error) { return 0, errors.New("private object response") }
func (failedObjectBody) Close() error             { return nil }

func TestUploadObjectFailureReasonPersistsWithoutLeakingOrProvingNoSend(t *testing.T) {
	for _, reason := range []string{"transport_error", "transport_timeout", "transport_dns", "transport_canceled", "object_http_status", "object_body_read", "object_body_limit", "object_etag_missing", "object_etag_invalid"} {
		t.Run(reason, func(t *testing.T) {
			calls := 0
			d := &Client{mediaTransport: transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				switch reason {
				case "transport_error":
					return nil, errors.New("private URL and token")
				case "transport_timeout":
					return nil, context.DeadlineExceeded
				case "transport_dns":
					return nil, &net.DNSError{Err: "private DNS error", Name: "private.example"}
				case "transport_canceled":
					return nil, context.Canceled
				}
				resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: r}
				switch reason {
				case "object_http_status":
					resp.StatusCode = 503
				case "object_body_read":
					resp.Body = failedObjectBody{}
				case "object_body_limit":
					resp.Body = io.NopCloser(strings.NewReader(strings.Repeat("x", apiLimit+1)))
				case "object_etag_invalid":
					resp.Header.Set("ETag", strings.Repeat("x", 1025))
				}
				return resp, nil
			})}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, "https://private.example/put?private-token", strings.NewReader("tiny"))
			_, err := d.sendUploadPart(req.Context(), req)
			if err == nil || calls != 1 {
				t.Fatal("failure fixture did not cross transport boundary")
			}
			saved := uploadSubmitFailure(err)
			if saved == nil || saved.Reason != reason {
				t.Fatalf("reason=%v want=%s", saved, reason)
			}
			p := uploadPayload{Phase: "put_inflight", SubmitFailure: saved}
			unknown := withUploadDiagnostic(cloud.OperationReceipt{ID: "private-receipt", State: "unknown"}, p, uploadUnknownError(p))
			diagnostic, ok := UploadDiagnosticFromError(unknown)
			if !ok || diagnostic.SubmitReason != reason || diagnostic.NotDispatched {
				t.Fatal("submit diagnostic implied replay safety or vanished")
			}
			if code, _ := cloud.ErrorInfo(unknown); code != cloud.CodeMutationUnknown {
				t.Fatal("diagnostic weakened unknown result")
			}
			if strings.Contains(unknown.Error(), "private") {
				t.Fatal("transport cause escaped")
			}
		})
	}
	p := uploadPayload{SubmitFailure: &operationSubmitFailure{Reason: "private arbitrary injected reason"}}
	err := withUploadDiagnostic(cloud.OperationReceipt{ID: "operation"}, p, errors.New("opaque failure"))
	if d, _ := UploadDiagnosticFromError(err); d.SubmitReason != "" {
		t.Fatal("non-allowlisted submit reason escaped")
	}
}
