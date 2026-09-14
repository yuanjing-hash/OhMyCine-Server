package nodeprotocol

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

const MaxHTTPResponseBytes = 8 << 20

// Responses bind the body, status and range metadata to the exact signed request.
// This protects HTTP manifests and task receipts as well as exported file chunks.
func responseMessage(req *http.Request, status int, headers http.Header, body []byte) []byte {
	digest := sha256.Sum256(body)
	fields := []string{"omc-node-http-response-v1", req.Header.Get("X-OhMyCine-Request-Signature"), req.Header.Get(HTTPNonceHeader), strconv.Itoa(status), hex.EncodeToString(digest[:])}
	for _, name := range []string{"Content-Type", "Content-Range", "ETag"} {
		fields = append(fields, name, headers.Get(name))
	}
	return []byte(strings.Join(fields, "\n"))
}

func SignHTTPResponse(req *http.Request, status int, headers http.Header, body, certificate []byte, key crypto.Signer) error {
	message := responseMessage(req, status, headers, body)
	var digest []byte
	var options crypto.SignerOpts = crypto.SHA256
	if _, ok := key.Public().(ed25519.PublicKey); ok {
		digest = message
		options = crypto.Hash(0)
	} else {
		sum := sha256.Sum256(message)
		digest = sum[:]
	}
	signature, err := key.Sign(rand.Reader, digest, options)
	if err != nil {
		return err
	}
	headers.Set("X-OhMyCine-Node-Certificate", base64.RawURLEncoding.EncodeToString(certificate))
	headers.Set("X-OhMyCine-Response-Signature", base64.RawURLEncoding.EncodeToString(signature))
	return nil
}

func VerifyHTTPResponse(req *http.Request, resp *http.Response, body []byte, fingerprint string) error {
	invalid := errors.New("node_response_signature_invalid")
	raw, err := base64.RawURLEncoding.DecodeString(resp.Header.Get("X-OhMyCine-Node-Certificate"))
	if err != nil || len(raw) > 8192 {
		return invalid
	}
	sum := sha256.Sum256(raw)
	if !SecureEqualHex(hex.EncodeToString(sum[:]), fingerprint) {
		return invalid
	}
	cert, err := x509.ParseCertificate(raw)
	if err != nil {
		return invalid
	}
	signature, err := base64.RawURLEncoding.DecodeString(resp.Header.Get("X-OhMyCine-Response-Signature"))
	if err != nil {
		return invalid
	}
	message := responseMessage(req, resp.StatusCode, resp.Header, body)
	digest := sha256.Sum256(message)
	switch key := cert.PublicKey.(type) {
	case ed25519.PublicKey:
		if ed25519.Verify(key, message, signature) {
			return nil
		}
	case *rsa.PublicKey:
		if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature) == nil {
			return nil
		}
	case *ecdsa.PublicKey:
		if ecdsa.VerifyASN1(key, digest[:], signature) {
			return nil
		}
	}
	return invalid
}
