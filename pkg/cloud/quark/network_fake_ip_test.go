package quark

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/netresolve"
	"golang.org/x/net/dns/dnsmessage"
)

func knownSyntheticDNS(context.Context, string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP("198.18.0.149")}, {IP: net.ParseIP("fdfe:dcba:9876::8d")}}, nil
}

func quarkDoHFixture(t *testing.T, calls *atomic.Int32, unsafe string) *http.Client {
	t.Helper()
	return &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.String() != "https://dns.alidns.com/dns-query" || r.Method != http.MethodPost || r.Header.Get("Accept") != "application/dns-message" || r.Header.Get("Content-Type") != "application/dns-message" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Agent-ID") != "" || r.Header.Get("X-Pan-Token") != "" {
			t.Error("controlled DNS query changed origin/protocol or leaked account headers")
		}
		wire, _ := io.ReadAll(r.Body)
		var query dnsmessage.Message
		if err := query.Unpack(wire); err != nil || len(query.Questions) != 1 || query.Questions[0].Name.String() != "example.com." {
			t.Fatal("DNS query contained more than the provider object hostname")
		}
		question := query.Questions[0]
		answer := dnsmessage.Message{Header: dnsmessage.Header{ID: query.ID, Response: true, RecursionAvailable: true}, Questions: query.Questions}
		if question.Type == dnsmessage.TypeA {
			address := [4]byte{8, 8, 8, 8}
			if unsafe == "private" {
				address = [4]byte{127, 0, 0, 1}
			} else if unsafe == "broadcast" {
				address = [4]byte{255, 255, 255, 255}
			}
			answer.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: question.Name, Type: question.Type, Class: dnsmessage.ClassINET, TTL: 30}, Body: &dnsmessage.AResource{A: address}}}
		}
		wire, _ = answer.Pack()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/dns-message"}}, Body: io.NopCloser(bytes.NewReader(wire)), Request: r}, nil
	})}
}

func TestCommonSpaceKnownFakeIPUsesPublicDNSForUploadReadbackAndCleanup(t *testing.T) {
	for _, scheme := range []string{"https", "http"} {
		t.Run(scheme, func(t *testing.T) {
			f := newProofRecoveryFixture(t, "")
			var puts, reads, dials, queries, systemCalls atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Host != "example.com" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Agent-ID") != "" || r.Header.Get("X-Pan-Token") != "" {
					t.Error("object request changed authority or leaked account credentials")
				}
				switch r.Method {
				case http.MethodPut:
					puts.Add(1)
					f.content, _ = io.ReadAll(io.LimitReader(r.Body, 33))
					if r.URL.Path != "/put" || len(f.content) != 32 || r.ContentLength != 32 || r.Header.Get("Authorization") != "object-only-signature" {
						t.Error("synthetic-DNS repair changed the exact challenge PUT")
					}
					w.Header().Set("ETag", "challenge-part")
				case http.MethodGet:
					reads.Add(1)
					if r.URL.Path != "/read" || r.Header.Get("Authorization") != "" || r.Header.Get("Range") != "bytes=0-31" {
						t.Error("TV proof/cleanup read lost its original anonymous range boundary")
					}
					w.Header().Set("Content-Range", "bytes 0-31/32")
					w.Header().Set("Content-Length", "32")
					w.WriteHeader(http.StatusPartialContent)
					_, _ = w.Write(f.content)
				default:
					t.Error("unexpected provider object method")
				}
			}))
			if scheme == "https" {
				server.StartTLS()
			} else {
				server.Start()
			}
			t.Cleanup(server.Close)
			transport := newPublicTransportWithDNS(func(ctx context.Context, name string) ([]net.IPAddr, error) {
				systemCalls.Add(1)
				return knownSyntheticDNS(ctx, name)
			}, quarkDoHFixture(t, &queries, ""), func(ctx context.Context, network, address string) (net.Conn, error) {
				dials.Add(1)
				if address != "8.8.8.8:443" && address != "8.8.8.8:80" {
					t.Error("synthetic or unvalidated address reached an object socket")
				}
				return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
			})
			if scheme == "https" {
				roots := x509.NewCertPool()
				roots.AddCert(server.Certificate())
				transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
			}
			transport.DialTLSContext = nil
			t.Cleanup(transport.CloseIdleConnections)
			d := f.client()
			d.mediaTransport = transport
			d.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/open/v1/file/upload_pre" {
					f.pre++
					var body struct {
						Name string `json:"file_name"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					f.name = body.Name
					return envelope(r, map[string]any{"task_id": "original-task", "part_size": 64, "upload_urls": []uploadDestination{objectTestDestination(scheme + "://example.com/put")}}), nil
				}
				if r.URL.Path == "/file" && r.URL.Query().Get("method") == "download" {
					return envelope(r, map[string]any{"fid": "tv-created", "file_name": f.name, "size": 32, "download_url": scheme + "://example.com/read"}), nil
				}
				return f.api(r)
			})
			proof, err := d.VerifyCommonSpace(context.Background(), "proof", "/媒体库")
			if err != nil || !f.credentials.HasProof(proof.TVRoot, proof.AgentRoot) || puts.Load() != 1 || reads.Load() != 2 || f.pre != 1 || f.finish != 1 || f.deletes != 1 || f.exists {
				t.Fatalf("known synthetic DNS blocked complete native proof: err=%v put=%d read=%d pre=%d finish=%d cleanup=%d", err, puts.Load(), reads.Load(), f.pre, f.finish, f.deletes)
			}
			if queries.Load() != 2 || systemCalls.Load() < 3 || dials.Load() != 3 {
				t.Fatal("shared resolver did not cache bounded public answers and recheck current system DNS at every dial")
			}
			var saved proofPayload
			_ = json.Unmarshal(f.store.receipts["proof"].Payload, &saved)
			if !bytes.Equal(saved.Bytes, f.content) || len(saved.Bytes) != 32 {
				t.Fatal("readback did not verify the original private challenge bytes")
			}
		})
	}
}

func TestQuarkObjectDNSPreservesNativeAndSharedAddressVetoes(t *testing.T) {
	for _, bad := range []string{"private", "unknown_ula", "broadcast", "zone", "malformed", "literal_fake", "unsafe_doh", "unsafe_doh_broadcast"} {
		t.Run(bad, func(t *testing.T) {
			var queries, dials atomic.Int32
			system := func(ctx context.Context, name string) ([]net.IPAddr, error) {
				addresses, _ := knownSyntheticDNS(ctx, name)
				switch bad {
				case "private":
					addresses = append(addresses, net.IPAddr{IP: net.ParseIP("10.0.0.1")})
				case "unknown_ula":
					addresses = append(addresses, net.IPAddr{IP: net.ParseIP("fdfe:dcba:9876:1::1")})
				case "broadcast":
					addresses = append(addresses, net.IPAddr{IP: net.ParseIP("255.255.255.255")})
				case "zone":
					addresses[0].Zone = "eth0"
				case "malformed":
					addresses = append(addresses, net.IPAddr{IP: net.IP{1, 2, 3}})
				}
				return addresses, nil
			}
			replacement := ""
			if bad == "unsafe_doh" {
				replacement = "private"
			} else if bad == "unsafe_doh_broadcast" {
				replacement = "broadcast"
			}
			transport := newPublicTransportWithDNS(system, quarkDoHFixture(t, &queries, replacement), func(context.Context, string, string) (net.Conn, error) {
				dials.Add(1)
				return nil, io.ErrUnexpectedEOF
			})
			t.Cleanup(transport.CloseIdleConnections)
			hostname := "example.com"
			if bad == "literal_fake" {
				hostname = "198.18.0.149"
			}
			d := &Client{mediaTransport: transport}
			_, err := d.putPart(context.Background(), objectTestDestination("https://"+hostname+"/put"), nil, strings.NewReader("tiny"), 4, Authorization{})
			if err == nil || dials.Load() != 0 || !strings.HasPrefix(bad, "unsafe_doh") && queries.Load() != 0 {
				t.Fatal("unrecognized unsafe DNS/literal reached fallback or an object socket")
			}
			if bad == "unsafe_doh" && queries.Load() != 1 {
				t.Fatal("unsafe replacement response did not fail immediately")
			}
			if bad == "unsafe_doh_broadcast" && queries.Load() != 2 {
				t.Fatal("native final predicate was not exercised after both public-baseline DNS responses")
			}
			if failure := uploadSubmitFailure(err); failure == nil || failure.Reason != "transport_address_rejected" {
				t.Fatal("address rejection lost its finite native diagnostic")
			}
			if netresolve.ErrorCode(err) != "" {
				t.Fatal("shared DNS internals escaped the native safe failure adapter")
			}
		})
	}
}

// A trusted issuer does not authorize a different object hostname. Leave
// ServerName unset so the native transport verifies the original URL host.
func TestQuarkObjectTransportRetainsOriginalTLSHostname(t *testing.T) {
	var requests, queries atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	t.Cleanup(server.Close)
	transport := newPublicTransportWithDNS(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}, quarkDoHFixture(t, &queries, ""), func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "8.8.8.8:443" {
			t.Error("unexpected object dial destination")
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	})
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	transport.DialTLSContext = nil
	t.Cleanup(transport.CloseIdleConnections)
	d := &Client{mediaTransport: transport}
	_, err := d.putPart(context.Background(), objectTestDestination("https://wrong.example/put"), nil, strings.NewReader("tiny"), 4, Authorization{})
	failure := uploadSubmitFailure(err)
	if err == nil || failure == nil || failure.Reason != "transport_tls" || requests.Load() != 0 || queries.Load() != 0 {
		t.Fatal("trusted certificate for the wrong object hostname reached provider HTTP or lost its safe TLS classification")
	}
}
