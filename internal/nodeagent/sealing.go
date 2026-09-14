package nodeagent

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func EnsureSealingIdentity(cfg Config) error {
	if info, err := os.Stat(cfg.SealingPrivateKeyFile); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("node_sealing_identity_invalid")
		}
		_, _, err := loadSealingIdentity(cfg.SealingPrivateKeyFile)
		return err
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("node_sealing_identity_unavailable")
	}
	if err := os.MkdirAll(filepath.Dir(cfg.SealingPrivateKeyFile), 0o700); err != nil {
		return errors.New("node_sealing_identity_directory_failed")
	}
	privateKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return errors.New("node_sealing_identity_generation_failed")
	}
	encoded := base64.RawURLEncoding.EncodeToString(privateKey.Bytes())
	return writeExclusive(cfg.SealingPrivateKeyFile, []byte(encoded+"\n"), 0o600)
}

func loadSealingIdentity(path string) (*ecdh.PrivateKey, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", errors.New("node_sealing_identity_unavailable")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, "", errors.New("node_sealing_identity_invalid")
	}
	privateKey, err := ecdh.X25519().NewPrivateKey(decoded)
	if err != nil {
		return nil, "", errors.New("node_sealing_identity_invalid")
	}
	publicKey := base64.RawURLEncoding.EncodeToString(privateKey.PublicKey().Bytes())
	return privateKey, publicKey, nil
}
