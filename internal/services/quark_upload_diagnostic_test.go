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
	fixture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/open/v1/file/list_uploaded_parts" {
			t.Error("unexpected provider call")
		}
		queries++
		_ = json.NewEncoder(w).Encode(map[string]any{"status": 0, "data": map[string]any{"finish_status": 0, "uploaded_parts": []any{}}})
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
	payload, _ := json.Marshal(map[string]any{"name": "private-name", "task_id": "private-task", "parent": "private-root", "size": 32, "part_size": 64, "phase": "put_inflight", "inflight_part": 1, "submit_failure": map[string]int{"http_status": 503}})
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
	if event["phase"] != "upload_part_recovery" || event["upload_phase"] != "put_inflight" || event["upload_receipt_state"] != "unknown" || event["upload_has_task"] != true || event["upload_has_output"] != false || event["upload_inflight_part"] != float64(1) || event["upload_task_pages"] != float64(1) || event["upload_task_listing_complete"] != true || event["upload_task_finish_status"] != float64(0) || event["provider_http_status"] != float64(503) {
		t.Fatalf("production manager lost bounded upload state: %s", output.String())
	}
	for _, secret := range []string{"private-operation", "private-task", "private-root", "private-account", "private-name", "access-secret", "refresh-secret", "open-api-drive"} {
		if strings.Contains(output.String(), secret) || strings.Contains(ErrorMessage(public), secret) {
			t.Fatal("private upload data escaped")
		}
	}
}
