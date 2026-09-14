package nodeagent

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

func TestHTTPResponseAuthenticationAndRequestBinding(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	fingerprint := hex.EncodeToString(sum[:])
	req := httptest.NewRequest(http.MethodGet, "http://node.example/node/v1/health", nil)
	req.Header.Set(nodeprotocol.HTTPNonceHeader, "request-1")
	req.Header.Set("X-OhMyCine-Request-Signature", "signature-1")
	body := []byte(`{"status":"ok"}`)
	response := &http.Response{StatusCode: 200, Header: http.Header{}}
	if err := nodeprotocol.SignHTTPResponse(req, response.StatusCode, response.Header, body, der, key); err != nil {
		t.Fatal(err)
	}
	if err := nodeprotocol.VerifyHTTPResponse(req, response, body, fingerprint); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"body", "status", "request", "range"} {
		altered := &http.Response{StatusCode: response.StatusCode, Header: response.Header.Clone()}
		request := req.Clone(req.Context())
		payload := body
		switch change {
		case "body":
			payload = []byte(`{"status":"completed"}`)
		case "status":
			altered.StatusCode = 201
		case "request":
			request.Header.Set(nodeprotocol.HTTPNonceHeader, "request-2")
		case "range":
			altered.Header.Set("Content-Range", "bytes 1-2/10")
		}
		if nodeprotocol.VerifyHTTPResponse(request, altered, payload, fingerprint) == nil {
			t.Fatalf("accepted changed %s", change)
		}
	}
}

func TestHTTPReplayNonceSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	agent := &Agent{store: store, now: func() time.Time { return now }}
	if !agent.consumeHTTPNonce("nonce") || agent.consumeHTTPNonce("nonce") {
		t.Fatal("nonce not one-use")
	}
	store.Close()
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	agent.store = reopened
	if agent.consumeHTTPNonce("nonce") {
		t.Fatal("restart accepted replay")
	}
	now = now.Add(3 * time.Minute)
	if !agent.consumeHTTPNonce("nonce") {
		t.Fatal("expired receipts not cleaned")
	}
}
