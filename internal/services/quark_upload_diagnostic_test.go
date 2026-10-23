package services

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

func TestQuarkRootUploadStateSurvivesProductionLogManager(t *testing.T) {
	queries := 0
	taskData := map[string]any{"finish_status": 0, "uploaded_parts": []any{}}
	fixture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/open/v1/file/list_uploaded_parts" {
			t.Error("unexpected provider call")
		}
		queries++
		_ = json.NewEncoder(w).Encode(map[string]any{"status": 0, "data": taskData})
	}))
	t.Cleanup(fixture.Close)
	previous := http.DefaultTransport
	transport := previous.(*http.Transport).Clone()
	roots := x509.NewCertPool()
	roots.AddCert(fixture.Certificate())
	config := &tls.Config{RootCAs: roots, ServerName: fixture.Certificate().DNSNames[0], MinVersion: tls.VersionTLS12}
	transport.Proxy = nil
	transport.DialTLSContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&tls.Dialer{NetDialer: &net.Dialer{}, Config: config}).DialContext(ctx, network, fixture.Listener.Addr().String())
	}
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previous; transport.CloseIdleConnections() })
	credentials, _ := json.Marshal(quark.Credentials{Version: 1, Revision: 1, Agent: testQuarkAuthorization()})
	payload, _ := json.Marshal(map[string]any{"name": "private-name", "task_id": "private-task", "parent": "private-root", "size": 32, "part_size": 64, "phase": "put_inflight", "inflight_part": 1, "submit_failure": map[string]any{"http_status": 503, "reason": "object_etag_missing"}})
	r := cloud.OperationReceipt{ID: "private-operation", Provider: cloud.ProviderQuark, Route: "agent", AccountID: "private-account", Kind: "upload", State: "unknown", Revision: 4, Payload: payload}
	store := &quarkReadbackOperations{rows: map[string]cloud.OperationReceipt{r.ID: r}}
	d, err := quark.NewClient(cloud.Config{Credentials: credentials, Operations: store})
	if err != nil {
		t.Fatal(err)
	}
	_, queryErr := d.QueryOperation(context.Background(), r.ID)
	if queryErr == nil || queries != 1 {
		t.Fatal("original pending task fixture not exercised")
	}
	var output bytes.Buffer
	manager, err := serverlog.NewManager(t.TempDir(), "test", &output)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	s := &ConnectionService{log: zerolog.New(manager)}
	public := s.quarkRootFailure(2, "challenge_upload", queryErr)
	var event map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &event); err != nil {
		t.Fatal(err)
	}
	if event["phase"] != "upload_part_recovery" || event["upload_phase"] != "put_inflight" || event["upload_receipt_state"] != "unknown" || event["upload_has_task"] != true || event["upload_has_output"] != false || event["upload_inflight_part"] != float64(1) || event["upload_task_pages"] != float64(1) || event["upload_task_listing_complete"] != true || event["upload_task_finish_status"] != float64(0) || event["upload_task_query_reason"] != "task_unfinished" || event["provider_http_status"] != float64(503) {
		t.Fatalf("production manager lost bounded upload state: %s", output.String())
	}
	if event["upload_submit_reason"] != "object_etag_missing" {
		t.Fatal("production manager lost finite first-submit reason")
	}
	for _, secret := range []string{"private-operation", "private-task", "private-root", "private-account", "private-name", "access-secret", "refresh-secret", "open-api-drive"} {
		if strings.Contains(output.String(), secret) || strings.Contains(ErrorMessage(public), secret) {
			t.Fatal("private upload data escaped")
		}
	}
	for _, tt := range []struct {
		name, field, valueType string
		data                   map[string]any
	}{
		{"status", "finish_status", "string", map[string]any{"finish_status": "private-provider-status"}},
		{"parts", "uploaded_parts", "object", map[string]any{"uploaded_parts": map[string]any{"private-upstream-field": "private-upstream-value"}}},
		{"marker", "next_part_number_marker", "object", map[string]any{"next_part_number_marker": map[string]any{"private-upstream-field": "private-upstream-value"}}},
		{"part_number", "uploaded_parts.part_number", "string", map[string]any{"uploaded_parts": []any{map[string]any{"part_number": "private-provider-part", "etag": "private-provider-etag"}}}},
		{"part_etag", "uploaded_parts.etag", "object", map[string]any{"uploaded_parts": []any{map[string]any{"part_number": 1, "etag": map[string]any{"private-upstream-field": "private-upstream-value"}}}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			taskData = tt.data
			_, queryErr := d.QueryOperation(context.Background(), r.ID)
			if queryErr == nil {
				t.Fatal("invalid task response was accepted")
			}
			output.Reset()
			public := s.quarkRootFailure(2, "challenge_upload", queryErr)
			var event map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &event); err != nil {
				t.Fatal(err)
			}
			if event["phase"] != "upload_task_query" || event["upload_task_response_field"] != tt.field || event["upload_task_response_type"] != tt.valueType || event["upload_task_pages"] != float64(1) || event["upload_task_listing_complete"] != false || event["upload_receipt_state"] != "unknown" || event["upload_has_task"] != true || event["upload_has_output"] != false || event["upload_inflight_part"] != float64(1) {
				t.Fatalf("production manager lost fixed response diagnosis: %s", output.String())
			}
			for _, secret := range []string{"private-", "access-secret", "refresh-secret", "open-api-drive"} {
				if strings.Contains(output.String(), secret) || strings.Contains(ErrorMessage(public), secret) {
					t.Fatal("private provider data escaped the response diagnostic")
				}
			}
			if len(store.rows) != 1 || !bytes.Equal(store.rows[r.ID].Payload, payload) || store.rows[r.ID].Revision != r.Revision {
				t.Fatal("invalid read-only query changed the original receipt")
			}
		})
	}
	// Admission applies to this read, not the historical unknown PUT. Exercise
	// the actual logger so the safe projection cannot become misleading later.
	beforeQueries := queries
	blocked, err := quark.NewClient(cloud.Config{Credentials: credentials, Operations: store, BeforeCall: func(context.Context) error {
		return context.DeadlineExceeded
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, queryErr = blocked.QueryOperation(context.Background(), r.ID)
	output.Reset()
	public = s.quarkRootFailure(2, "challenge_upload", queryErr)
	if queryErr == nil || json.Unmarshal(bytes.TrimSpace(output.Bytes()), &event) != nil {
		t.Fatal("read admission failure was not logged")
	}
	if event["phase"] != "upload_task_query" || event["upload_phase"] != "put_inflight" || event["upload_receipt_state"] != "unknown" || event["upload_not_dispatched"] != false || event["upload_task_pages"] != float64(0) || queries != beforeQueries {
		t.Fatalf("read admission was mistaken for original write evidence: %s", output.String())
	}
	if strings.Contains(output.String(), "private") || strings.Contains(ErrorMessage(public), "private") || len(store.rows) != 1 || !bytes.Equal(store.rows[r.ID].Payload, payload) || store.rows[r.ID].Revision != r.Revision {
		t.Fatal("blocked query leaked or rewrote the original upload")
	}
}
