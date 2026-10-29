package nodeprotocol

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
)

// The ciphertext can travel over either supported Node transport. Its random
// key travels only in the separately sealed task/operation credential grant.
type DownloaderSourceEnvelope struct {
	Digest             string `json:"digest"`
	SelectionDigest    string `json:"selection_digest"`
	ConfigurationScope string `json:"configuration_scope"`
	Nonce              []byte `json:"nonce,omitempty"`
	Ciphertext         []byte `json:"ciphertext,omitempty"`
}

type DownloaderSourceBinding struct{ ServerID, TaskID, OperationKey, DownloaderID, SavePath, Tag, ConfigurationScope string }

func sourceAAD(envelope *DownloaderSourceEnvelope, binding DownloaderSourceBinding) []byte {
	raw, _ := json.Marshal(struct {
		Version                 string
		Digest, SelectionDigest string
		Binding                 DownloaderSourceBinding
	}{"omc-downloader-source-v1", envelope.Digest, envelope.SelectionDigest, binding})
	return raw
}

func SealDownloaderSource(source DownloaderSource, binding DownloaderSourceBinding) (*DownloaderSourceEnvelope, string, error) {
	raw, err := json.Marshal(source)
	if err != nil || len(raw) > MaxWireBodyBytes {
		return nil, "", errors.New("node_selection_source_too_large")
	}
	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		return nil, "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, "", err
	}
	digest := digestBytes(raw)
	if source.ContentSelection == nil || !ValidDigest(binding.ConfigurationScope) {
		return nil, "", errors.New("node_selection_source_invalid")
	}
	selectionDigest, err := source.ContentSelection.Digest()
	if err != nil {
		return nil, "", err
	}
	envelope := &DownloaderSourceEnvelope{Digest: digest, SelectionDigest: selectionDigest, ConfigurationScope: binding.ConfigurationScope, Nonce: nonce}
	envelope.Ciphertext = aead.Seal(nil, nonce, raw, sourceAAD(envelope, binding))
	return envelope, base64.RawStdEncoding.EncodeToString(key), nil
}
func OpenDownloaderSource(envelope *DownloaderSourceEnvelope, encodedKey string, binding DownloaderSourceBinding) (DownloaderSource, error) {
	var source DownloaderSource
	if envelope == nil || !ValidDigest(envelope.Digest) || !ValidDigest(envelope.SelectionDigest) || !ValidDigest(binding.ConfigurationScope) || binding.ConfigurationScope != envelope.ConfigurationScope || len(envelope.Nonce) != 12 || len(envelope.Ciphertext) < 16 || len(envelope.Ciphertext) > MaxWireBodyBytes {
		return source, errors.New("node_selection_source_invalid")
	}
	key, err := base64.RawStdEncoding.DecodeString(encodedKey)
	if err != nil || len(key) != 32 {
		return source, errors.New("node_selection_source_invalid")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return source, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return source, err
	}
	raw, err := aead.Open(nil, envelope.Nonce, envelope.Ciphertext, sourceAAD(envelope, binding))
	if err != nil || digestBytes(raw) != envelope.Digest || json.Unmarshal(raw, &source) != nil {
		return source, errors.New("node_selection_source_invalid")
	}
	if source.ContentSelection == nil {
		return source, errors.New("node_selection_source_invalid")
	}
	digest, err := source.ContentSelection.Digest()
	if err != nil || digest != envelope.SelectionDigest {
		return source, errors.New("node_selection_source_invalid")
	}
	return source, nil
}
