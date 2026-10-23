package quark

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

func TestUploadNativePublicDialFailureRetainsFiniteReason(t *testing.T) {
	for _, tt := range []struct {
		name, reason string
		lookupErr    error
		dialErr      error
		answers      []net.IPAddr
		wantDials    int
	}{
		{name: "dns", reason: "transport_dns", lookupErr: &net.DNSError{Err: "private resolver text", Name: "private-host.example"}},
		{name: "empty_dns", reason: "transport_dns"},
		{name: "dns_timeout", reason: "transport_timeout", lookupErr: &net.DNSError{Err: "private resolver text", Name: "private-host.example", IsTimeout: true}},
		{name: "private_answer", reason: "transport_address_rejected", answers: []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("127.0.0.1")}}},
		{name: "injected_synthetic_answer", reason: "transport_address_rejected", answers: []net.IPAddr{{IP: net.ParseIP("198.18.0.149")}}},
		{name: "connect", reason: "transport_connect", answers: []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, dialErr: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, wantDials: 1},
		{name: "connect_timeout", reason: "transport_timeout", answers: []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, dialErr: context.DeadlineExceeded, wantDials: 1},
		{name: "connect_canceled", reason: "transport_canceled", answers: []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, dialErr: context.Canceled, wantDials: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dials := 0
			transport := newPublicTransport()
			transport.DialContext = publicDialContext(func(context.Context, string) ([]net.IPAddr, error) {
				return tt.answers, tt.lookupErr
			}, func(context.Context, string, string) (net.Conn, error) {
				dials++
				return nil, tt.dialErr
			})
			t.Cleanup(transport.CloseIdleConnections)
			d := &Client{mediaTransport: transport}
			request, err := prepareUploadPartRequest(context.Background(), objectTestDestination("https://private-host.example/put?private-signature"), nil, strings.NewReader("tiny"), 4, Authorization{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = d.sendUploadPart(request.Context(), request)
			if err == nil || dials != tt.wantDials {
				t.Fatalf("native transport boundary was not exercised: dials=%d want=%d", dials, tt.wantDials)
			}
			saved := uploadSubmitFailure(err)
			if saved == nil || saved.Reason != tt.reason {
				t.Fatalf("native dial cause was erased: got=%+v want=%s", saved, tt.reason)
			}
			p := uploadPayload{Phase: "put_inflight", InFlightPart: 1, SubmitFailure: saved}
			unknown := withUploadDiagnostic(cloud.OperationReceipt{ID: "private-operation", State: "unknown"}, p, uploadUnknownError(p))
			diagnostic, ok := UploadDiagnosticFromError(unknown)
			if !ok || diagnostic.NotDispatched || diagnostic.SubmitReason != tt.reason {
				t.Fatal("a transport diagnostic implied permission to replay")
			}
			if code, _ := cloud.ErrorInfo(unknown); code != cloud.CodeMutationUnknown || strings.Contains(unknown.Error(), "private") {
				t.Fatal("native cause leaked or changed the unknown outcome")
			}
		})
	}
}

func objectTestDestination(raw string) uploadDestination {
	destination := uploadDestination{URL: raw}
	destination.Signature.Value = "object-only-signature"
	return destination
}

// Keep the native transport's HTTP/TLS behavior and public-address guard. Only
// name resolution and the final socket destination are mapped to this local
// server; its certificate must still verify against the explicit test root.
func localPublicObjectTransport(t *testing.T, server *httptest.Server) *http.Transport {
	t.Helper()
	transport := newPublicTransport()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, ServerName: server.Certificate().DNSNames[0], MinVersion: tls.VersionTLS12}
	transport.DialTLSContext = nil
	transport.DialContext = publicDialContext(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "8.8.8.8:443" {
			t.Errorf("unexpected validated dial target: %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	})
	t.Cleanup(transport.CloseIdleConnections)
	return transport
}

func TestUploadNativePublicTransportTLSWireAndUnknownReceipt(t *testing.T) {
	for _, mode := range []string{"http1", "http2", "lost_ack", "http2_lost_ack"} {
		t.Run(mode, func(t *testing.T) {
			f := newProofRecoveryFixture(t, "")
			f.name = ""
			puts := 0
			http2, lostACK := strings.HasPrefix(mode, "http2"), strings.HasSuffix(mode, "lost_ack")
			const requestURI = "/object%2Fpart?uploadId=private%2Bcapability&partNumber=1"
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				puts++
				data, err := io.ReadAll(io.LimitReader(r.Body, 33))
				if err != nil || r.Method != http.MethodPut || r.RequestURI != requestURI || len(data) != 32 || r.ContentLength != 32 || len(r.TransferEncoding) != 0 {
					t.Error("native object PUT changed URI or fixed challenge framing")
				}
				if r.Header.Get("Authorization") != "object-only-signature" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Agent-ID") != "" || r.Header.Get("X-Pan-Token") != "" || r.Header.Get("X-Object-Signed") != "signed-value" {
					t.Error("native object PUT changed signed headers or leaked account credentials")
				}
				if http2 && r.ProtoMajor != 2 || !http2 && r.ProtoMajor != 1 {
					t.Error("native TLS fixture did not negotiate the expected protocol")
				}
				f.put++
				f.content = append([]byte(nil), data...)
				if lostACK {
					if http2 {
						panic(http.ErrAbortHandler)
					}
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
				w.Header().Set("ETag", "\"quoted-original-ack\"")
			}))
			server.EnableHTTP2 = http2
			server.StartTLS()
			t.Cleanup(server.Close)
			transport := localPublicObjectTransport(t, server)
			client := func() *Client {
				d := f.client()
				readTransport := d.mediaTransport
				d.mediaTransport = transportFunc(func(r *http.Request) (*http.Response, error) {
					if r.Method == http.MethodPut {
						return transport.RoundTrip(r)
					}
					return readTransport.RoundTrip(r)
				})
				d.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/open/v1/file/upload_pre" {
						f.pre++
						var body struct {
							Name string `json:"file_name"`
						}
						_ = json.NewDecoder(r.Body).Decode(&body)
						f.name = body.Name
						return envelope(r, map[string]any{"task_id": "original-task", "part_size": 64, "common_headers": map[string]string{"X-Object-Signed": "signed-value"}, "upload_urls": []uploadDestination{objectTestDestination("https://objects.example" + requestURI)}}), nil
					}
					if r.URL.Path == "/open/v1/file/upload_finish" {
						var body struct {
							Parts []uploadPart `json:"part_info_list"`
						}
						_ = json.NewDecoder(r.Body).Decode(&body)
						if len(body.Parts) != 1 || body.Parts[0].ETag != "\"quoted-original-ack\"" {
							t.Error("finish lost the original quoted object acknowledgement")
						}
					}
					return f.api(r)
				})
				return d
			}
			// This fixture tests the upload and its durable receipt; TV content
			// reads retain the existing full-content fixture rather than being
			// mistaken for a second object PUT at this server.
			d := client()
			proof, err := d.VerifyCommonSpace(context.Background(), "proof", "/媒体库")
			if lostACK {
				errorCode(t, err, cloud.CodeMutationUnknown)
				original := f.store.receipts["proof/upload"]
				for i := 0; i < 2; i++ {
					_, err = client().VerifyCommonSpace(context.Background(), "proof", "/媒体库")
					errorCode(t, err, cloud.CodeMutationUnknown)
				}
				current := f.store.receipts["proof/upload"]
				if current.State != "unknown" || !bytes.Equal(current.Payload, original.Payload) || current.Revision != original.Revision || f.pre != 1 || puts != 1 || f.finish != 0 {
					t.Fatal("cold unknown-ACK recovery changed or replayed the original PUT")
				}
				return
			}
			if err != nil || !f.credentials.HasProof(proof.TVRoot, proof.AgentRoot) || puts != 1 || f.pre != 1 || f.finish != 1 {
				t.Fatalf("native fixed-body TLS PUT failed: %v pre=%d puts=%d finish=%d", err, f.pre, puts, f.finish)
			}
		})
	}
}

func TestUploadNativeTLSVerificationRemainsRequired(t *testing.T) {
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	t.Cleanup(server.Close)
	transport := localPublicObjectTransport(t, server)
	transport.TLSClientConfig.RootCAs = x509.NewCertPool()
	d := &Client{mediaTransport: transport}
	_, err := d.putPart(context.Background(), objectTestDestination("https://objects.example/put"), nil, strings.NewReader("tiny"), 4, Authorization{})
	var f *Failure
	if !errors.As(err, &f) || f.SubmitReason != "transport_tls" || requests != 0 {
		t.Fatal("untrusted object certificate was accepted or lost its finite reason")
	}
}
