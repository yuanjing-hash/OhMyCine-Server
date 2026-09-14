package nodeprotocol

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/hkdf"
)

const MaxCredentialGrantBytes = 64 << 10

type CredentialGrant struct {
	GrantID            string          `json:"grant_id"`
	NodeID             string          `json:"node_id"`
	TaskID             string          `json:"task_id"`
	OperationKey       string          `json:"operation_key"`
	ResourceKind       string          `json:"resource_kind"`
	ResourceID         string          `json:"resource_id"`
	CredentialRevision uint64          `json:"credential_revision"`
	AllowedActions     []string        `json:"allowed_actions"`
	ExpiresAt          time.Time       `json:"expires_at"`
	Credential         json.RawMessage `json:"credential"`
}

type CredentialGrantEnvelope struct {
	GrantID            string    `json:"grant_id"`
	NodeID             string    `json:"node_id"`
	TaskID             string    `json:"task_id"`
	OperationKey       string    `json:"operation_key"`
	ResourceKind       string    `json:"resource_kind"`
	ResourceID         string    `json:"resource_id"`
	CredentialRevision uint64    `json:"credential_revision"`
	ExpiresAt          time.Time `json:"expires_at"`
	EphemeralPublicKey string    `json:"ephemeral_public_key"`
	Nonce              string    `json:"nonce"`
	Ciphertext         string    `json:"ciphertext"`
}

func (grant CredentialGrant) Validate(now time.Time) error {
	if !validID(grant.GrantID) || !validID(grant.NodeID) || !validID(grant.TaskID) || !validID(grant.OperationKey) || !validID(grant.ResourceKind) || !validID(grant.ResourceID) || grant.CredentialRevision == 0 {
		return errors.New("node_credential_grant_invalid")
	}
	if !grant.ExpiresAt.After(now) || grant.ExpiresAt.After(now.Add(15*time.Minute)) {
		return errors.New(ErrorCredentialExpired)
	}
	if len(grant.Credential) == 0 || len(grant.Credential) > MaxCredentialGrantBytes || !json.Valid(grant.Credential) {
		return errors.New("node_credential_payload_invalid")
	}
	if len(grant.AllowedActions) == 0 || len(grant.AllowedActions) > 16 {
		return errors.New("node_credential_actions_invalid")
	}
	seen := make(map[string]struct{}, len(grant.AllowedActions))
	for _, action := range grant.AllowedActions {
		if !validID(action) {
			return errors.New("node_credential_actions_invalid")
		}
		if _, exists := seen[action]; exists {
			return errors.New("node_credential_actions_invalid")
		}
		seen[action] = struct{}{}
	}
	return nil
}

func SealCredentialGrant(nodePublicKey string, grant CredentialGrant, now time.Time) (CredentialGrantEnvelope, error) {
	if err := grant.Validate(now); err != nil {
		return CredentialGrantEnvelope{}, err
	}
	publicBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(nodePublicKey))
	if err != nil {
		return CredentialGrantEnvelope{}, errors.New("node_encryption_public_key_invalid")
	}
	publicKey, err := ecdh.X25519().NewPublicKey(publicBytes)
	if err != nil {
		return CredentialGrantEnvelope{}, errors.New("node_encryption_public_key_invalid")
	}
	ephemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return CredentialGrantEnvelope{}, err
	}
	shared, err := ephemeral.ECDH(publicKey)
	if err != nil {
		return CredentialGrantEnvelope{}, errors.New("node_credential_seal_failed")
	}
	grant.AllowedActions = append([]string(nil), grant.AllowedActions...)
	sort.Strings(grant.AllowedActions)
	credential, err := canonicalJSON(grant.Credential)
	if err != nil {
		return CredentialGrantEnvelope{}, errors.New("node_credential_payload_invalid")
	}
	grant.Credential = credential
	plaintext, err := json.Marshal(grant)
	if err != nil {
		return CredentialGrantEnvelope{}, err
	}
	envelope := CredentialGrantEnvelope{
		GrantID:            grant.GrantID,
		NodeID:             grant.NodeID,
		TaskID:             grant.TaskID,
		OperationKey:       grant.OperationKey,
		ResourceKind:       grant.ResourceKind,
		ResourceID:         grant.ResourceID,
		CredentialRevision: grant.CredentialRevision,
		ExpiresAt:          grant.ExpiresAt.UTC(),
		EphemeralPublicKey: base64.RawURLEncoding.EncodeToString(ephemeral.PublicKey().Bytes()),
	}
	aead, err := grantAEAD(shared, grantAAD(envelope))
	if err != nil {
		return CredentialGrantEnvelope{}, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return CredentialGrantEnvelope{}, err
	}
	envelope.Nonce = base64.RawURLEncoding.EncodeToString(nonce)
	envelope.Ciphertext = base64.RawURLEncoding.EncodeToString(aead.Seal(nil, nonce, plaintext, grantAAD(envelope)))
	return envelope, nil
}

func OpenCredentialGrant(privateKey *ecdh.PrivateKey, envelope CredentialGrantEnvelope, now time.Time) (CredentialGrant, error) {
	if privateKey == nil || !validID(envelope.GrantID) || !validID(envelope.NodeID) || !validID(envelope.TaskID) || !validID(envelope.OperationKey) || !validID(envelope.ResourceKind) || !validID(envelope.ResourceID) || envelope.CredentialRevision == 0 || !envelope.ExpiresAt.After(now) {
		return CredentialGrant{}, errors.New(ErrorCredentialExpired)
	}
	ephBytes, err := base64.RawURLEncoding.DecodeString(envelope.EphemeralPublicKey)
	if err != nil {
		return CredentialGrant{}, errors.New("node_credential_envelope_invalid")
	}
	ephemeral, err := ecdh.X25519().NewPublicKey(ephBytes)
	if err != nil {
		return CredentialGrant{}, errors.New("node_credential_envelope_invalid")
	}
	shared, err := privateKey.ECDH(ephemeral)
	if err != nil {
		return CredentialGrant{}, errors.New("node_credential_envelope_invalid")
	}
	aead, err := grantAEAD(shared, grantAAD(envelope))
	if err != nil {
		return CredentialGrant{}, err
	}
	nonce, err := base64.RawURLEncoding.DecodeString(envelope.Nonce)
	if err != nil || len(nonce) != aead.NonceSize() {
		return CredentialGrant{}, errors.New("node_credential_envelope_invalid")
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(envelope.Ciphertext)
	if err != nil || len(ciphertext) > MaxCredentialGrantBytes+(16<<10) {
		return CredentialGrant{}, errors.New("node_credential_envelope_invalid")
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, grantAAD(envelope))
	if err != nil {
		return CredentialGrant{}, errors.New("node_credential_envelope_invalid")
	}
	var grant CredentialGrant
	decoder := json.NewDecoder(bytes.NewReader(plaintext))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&grant); err != nil {
		return CredentialGrant{}, errors.New("node_credential_envelope_invalid")
	}
	if err := grant.Validate(now); err != nil {
		return CredentialGrant{}, err
	}
	if grant.GrantID != envelope.GrantID || grant.NodeID != envelope.NodeID || grant.TaskID != envelope.TaskID || grant.OperationKey != envelope.OperationKey || grant.ResourceKind != envelope.ResourceKind || grant.ResourceID != envelope.ResourceID || grant.CredentialRevision != envelope.CredentialRevision || !grant.ExpiresAt.Equal(envelope.ExpiresAt) {
		return CredentialGrant{}, errors.New("node_credential_binding_mismatch")
	}
	return grant, nil
}

func grantAEAD(shared, aad []byte) (cipher.AEAD, error) {
	reader := hkdf.New(sha256.New, shared, nil, append([]byte("ohmycine-node-credential-grant-v1\n"), aad...))
	key := make([]byte, 32)
	if _, err := io.ReadFull(reader, key); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func grantAAD(envelope CredentialGrantEnvelope) []byte {
	return []byte(strings.Join([]string{
		"ohmycine-node-credential-grant-v1",
		envelope.GrantID,
		envelope.NodeID,
		envelope.TaskID,
		envelope.OperationKey,
		envelope.ResourceKind,
		envelope.ResourceID,
		strconv.FormatUint(envelope.CredentialRevision, 10),
		envelope.ExpiresAt.UTC().Format(time.RFC3339Nano),
		envelope.EphemeralPublicKey,
	}, "\n"))
}
