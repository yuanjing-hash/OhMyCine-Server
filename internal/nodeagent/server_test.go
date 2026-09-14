package nodeagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

func TestAgentHealthAndOperationIdempotency(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	agent, err := New(Config{NodeID: "node-1", ListenAddress: "127.0.0.1:0", DataDirectory: dir, ManagedRoot: filepath.Join(dir, "managed"), MaxConcurrentOperations: 2, AllowInsecureDevelopment: true}, store)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(agent.Handler())
	defer server.Close()
	response, err := http.Get(server.URL + "/node/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status %d", response.StatusCode)
	}
	now := time.Now().UTC()
	plan := nodeprotocol.OperationPlan{ProtocolVersion: 1, OperationKey: "op-1", TaskID: "task-1", NodeID: "node-1", Kind: "test", PlanRevision: 1, LeaseEpoch: 1, LeaseExpiresAt: now.Add(time.Minute), Payload: json.RawMessage(`{}`)}
	digest, _ := plan.Digest()
	body, _ := json.Marshal(nodeprotocol.PutOperationRequest{Plan: plan, PlanDigest: digest})
	for index, expected := range []int{http.StatusCreated, http.StatusOK} {
		req, _ := http.NewRequest(http.MethodPut, server.URL+"/node/v1/operations/op-1", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-OhMyCine-Server-ID", "server-1")
		got, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = got.Body.Close()
		if got.StatusCode != expected {
			t.Fatalf("request %d status=%d expected=%d", index, got.StatusCode, expected)
		}
	}
}

func TestPairedAgentHealthRequiresThePinnedServerIdentity(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	now := time.Now().UTC()
	serverCertificate := []byte("paired-server-certificate")
	serverSum := sha256.Sum256(serverCertificate)
	serverFingerprint := fmt.Sprintf("%x", serverSum[:])
	if err := store.SaveChallenge(context.Background(), "nonce-1", "server-1", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteEnrollment(context.Background(), "nonce-1", "server-1", serverFingerprint, fmt.Sprintf("%064x", 1), now); err != nil {
		t.Fatal(err)
	}
	config := Config{NodeID: "node-1", ListenAddress: "127.0.0.1:4433", DataDirectory: dir, ManagedRoot: filepath.Join(dir, "managed"), TLSCertificateFile: "node.crt", TLSPrivateKeyFile: "node.key", SealingPrivateKeyFile: filepath.Join(dir, "node.seal.key"), MaxConcurrentOperations: 2}
	if err := EnsureSealingIdentity(config); err != nil {
		t.Fatal(err)
	}
	agent, err := New(config, store)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "https://node.example.com/node/v1/health", nil)
	request.Header.Set("X-OhMyCine-Server-ID", "server-1")
	request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Raw: serverCertificate}}}
	recorder := httptest.NewRecorder()
	agent.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !bytes.Contains(recorder.Body.Bytes(), []byte(`"node_id":"node-1"`)) {
		t.Fatalf("paired health status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	wrong := httptest.NewRequest(http.MethodGet, "https://node.example.com/node/v1/health", nil)
	wrong.Header.Set("X-OhMyCine-Server-ID", "server-2")
	wrong.TLS = request.TLS
	wrongRecorder := httptest.NewRecorder()
	agent.Handler().ServeHTTP(wrongRecorder, wrong)
	if wrongRecorder.Code != http.StatusForbidden || bytes.Contains(wrongRecorder.Body.Bytes(), []byte("node-1")) {
		t.Fatalf("wrong identity status=%d body=%s", wrongRecorder.Code, wrongRecorder.Body.String())
	}

	challenge := httptest.NewRequest(http.MethodPost, "https://node.example.com/node/v1/enrollment/challenge", bytes.NewReader([]byte(`{}`)))
	challengeRecorder := httptest.NewRecorder()
	agent.Handler().ServeHTTP(challengeRecorder, challenge)
	if challengeRecorder.Code != http.StatusForbidden {
		t.Fatalf("paired node reopened enrollment: status=%d body=%s", challengeRecorder.Code, challengeRecorder.Body.String())
	}
}
