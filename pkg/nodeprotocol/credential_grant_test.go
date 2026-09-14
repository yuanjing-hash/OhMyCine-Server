package nodeprotocol

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func TestCredentialGrantRoundTripAndBinding(t *testing.T) {
	privateKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	grant := CredentialGrant{GrantID: "grant-1", NodeID: "node-1", TaskID: "task-1", OperationKey: "operation-1", ResourceKind: "downloader", ResourceID: "downloader-1", CredentialRevision: 2, AllowedActions: []string{"status", "submit"}, ExpiresAt: now.Add(10 * time.Minute), Credential: json.RawMessage(`{"base_url":"https://qb.example.com","username":"u","password":"secret"}`)}
	envelope, err := SealCredentialGrant(base64.RawURLEncoding.EncodeToString(privateKey.PublicKey().Bytes()), grant, now)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Ciphertext == "" || envelope.Ciphertext == string(grant.Credential) {
		t.Fatal("credential was not sealed")
	}
	opened, err := OpenCredentialGrant(privateKey, envelope, now)
	if err != nil {
		t.Fatal(err)
	}
	var credential map[string]string
	if err := json.Unmarshal(opened.Credential, &credential); err != nil {
		t.Fatal(err)
	}
	if opened.NodeID != grant.NodeID || opened.ResourceID != grant.ResourceID || credential["password"] != "secret" {
		t.Fatalf("opened grant=%+v", opened)
	}
	tampered := envelope
	tampered.TaskID = "task-2"
	if _, err := OpenCredentialGrant(privateKey, tampered, now); err == nil {
		t.Fatal("tampered task binding was accepted")
	}
	if _, err := OpenCredentialGrant(privateKey, envelope, envelope.ExpiresAt); err == nil {
		t.Fatal("expired grant was accepted")
	}
}
