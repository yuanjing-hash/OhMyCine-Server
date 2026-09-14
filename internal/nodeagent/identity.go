package nodeagent

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

func EnsureTLSIdentity(cfg Config) error {
	certInfo, certErr := os.Stat(cfg.TLSCertificateFile)
	keyInfo, keyErr := os.Stat(cfg.TLSPrivateKeyFile)
	if certErr == nil && keyErr == nil && certInfo.Mode().IsRegular() && keyInfo.Mode().IsRegular() {
		return nil
	}
	if !(errors.Is(certErr, os.ErrNotExist) && errors.Is(keyErr, os.ErrNotExist)) {
		return errors.New("node_tls_identity_incomplete")
	}
	if err := os.MkdirAll(filepath.Dir(cfg.TLSPrivateKeyFile), 0o700); err != nil {
		return errors.New("node_tls_identity_directory_failed")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return errors.New("node_tls_identity_generation_failed")
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return errors.New("node_tls_identity_generation_failed")
	}
	now := time.Now().UTC()
	template := x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "OhMyCine Node " + cfg.NodeID}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(5, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, pub, priv)
	if err != nil {
		return errors.New("node_tls_identity_generation_failed")
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return errors.New("node_tls_identity_generation_failed")
	}
	if err := writeExclusive(cfg.TLSPrivateKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	if err := writeExclusive(cfg.TLSCertificateFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		_ = os.Remove(cfg.TLSPrivateKeyFile)
		return err
	}
	return nil
}

func writeExclusive(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return errors.New("node_tls_identity_publish_failed")
	}
	name := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return errors.New("node_tls_identity_publish_failed")
	}
	if err = f.Sync(); err != nil {
		return errors.New("node_tls_identity_publish_failed")
	}
	if err = f.Close(); err != nil {
		return errors.New("node_tls_identity_publish_failed")
	}
	ok = true
	return nil
}
