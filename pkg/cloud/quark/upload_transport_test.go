package quark

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUploadProviderHTTPAndHTTPSPreserveObjectBoundary(t *testing.T) {
	for _, secure := range []bool{false, true} {
		name := "http"
		if secure {
			name = "https"
		}
		t.Run(name, func(t *testing.T) {
			calls, redirected := 0, 0
			payload := bytes.Repeat([]byte("q"), 32)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/redirect-target" {
					redirected++
					return
				}
				calls++
				data, _ := io.ReadAll(r.Body)
				if r.Method != http.MethodPut || r.ContentLength != 32 || !bytes.Equal(data, payload) || r.Header.Get("Authorization") != "object-signature" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Agent-ID") != "" || r.Header.Get("X-Pan-Token") != "" || r.Header.Get("X-Object-Header") != "object-value" {
					t.Error("object request lost its fixed body or credential boundary")
				}
				if r.URL.Path == "/redirect" {
					w.Header().Set("Location", "/redirect-target")
					w.WriteHeader(http.StatusTemporaryRedirect)
					return
				}
				w.Header().Set("ETag", "part-ack")
			}))
			if secure {
				server.StartTLS()
			} else {
				server.Start()
			}
			defer server.Close()
			d := &Client{mediaTransport: server.Client().Transport}
			a := Authorization{AccessToken: "private-access", RefreshToken: "private-refresh"}
			destination := uploadDestination{URL: server.URL + "/part"}
			destination.Signature.Value = "object-signature"
			common := map[string]string{"X-Object-Header": "object-value"}
			etag, err := d.putPart(context.Background(), destination, common, bytes.NewReader(payload), 32, a)
			if err != nil || etag != "part-ack" || calls != 1 {
				t.Fatalf("provider %s endpoint refused: %v", name, err)
			}
			destination.URL = server.URL + "/redirect"
			if _, err = d.putPart(context.Background(), destination, common, bytes.NewReader(payload), 32, a); err == nil || calls != 2 || redirected != 0 {
				t.Fatal("object upload followed a redirect or claimed success")
			}
		})
	}
}
