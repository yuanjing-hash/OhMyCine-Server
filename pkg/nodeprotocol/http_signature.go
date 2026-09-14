package nodeprotocol

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const HTTPNonceHeader = "X-OhMyCine-Request-Nonce"
const HTTPTimeHeader = "X-OhMyCine-Request-Time"

func HTTPEnrollmentProof(token string, input any) string {
	data, _ := json.Marshal(input)
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte("omc-node-http-enrollment-v1\n"))
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

// SignHTTPRequest retains controller authentication when HTTP is explicitly
// configured. It does not claim confidentiality for the HTTP connection.
func SignHTTPRequest(r *http.Request, certificate []byte, key ed25519.PrivateKey, nodeFingerprint string, now time.Time) error {
	if len(key) != ed25519.PrivateKeySize {
		return errors.New("node_signing_identity_invalid")
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	r.Header.Set(HTTPNonceHeader, hex.EncodeToString(nonce))
	r.Header.Set(HTTPTimeHeader, strconv.FormatInt(now.UnixNano(), 10))
	r.Header.Set("X-OhMyCine-Controller-Certificate", base64.RawURLEncoding.EncodeToString(certificate))
	message, err := httpSigningMessage(r, nodeFingerprint)
	if err != nil {
		return err
	}
	r.Header.Set("X-OhMyCine-Request-Signature", base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, message)))
	return nil
}

func VerifyHTTPRequest(r *http.Request, serverFingerprint, nodeFingerprint string, now, started time.Time) error {
	invalid := errors.New("node_request_signature_invalid")
	stamp, err := strconv.ParseInt(r.Header.Get(HTTPTimeHeader), 10, 64)
	if err != nil {
		return invalid
	}
	at := time.Unix(0, stamp)
	if at.Before(started) || at.Before(now.Add(-time.Minute)) || at.After(now.Add(time.Minute)) || !ValidDigest(r.Header.Get(HTTPNonceHeader)) {
		return invalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-OhMyCine-Controller-Certificate"))
	if err != nil || len(raw) > 8192 {
		return invalid
	}
	digest := sha256.Sum256(raw)
	if !SecureEqualHex(hex.EncodeToString(digest[:]), serverFingerprint) {
		return invalid
	}
	cert, err := x509.ParseCertificate(raw)
	if err != nil {
		return invalid
	}
	key, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		return invalid
	}
	signature, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-OhMyCine-Request-Signature"))
	if err != nil {
		return invalid
	}
	message, err := httpSigningMessage(r, nodeFingerprint)
	if err != nil || !ed25519.Verify(key, message, signature) {
		return invalid
	}
	return nil
}

func httpSigningMessage(r *http.Request, nodeFingerprint string) ([]byte, error) {
	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, (4<<20)+1))
		r.Body.Close()
		if err != nil || len(body) > 4<<20 {
			return nil, errors.New("node_request_body_invalid")
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	digest := sha256.Sum256(body)
	fields := []string{"omc-node-http-v1", nodeFingerprint, r.Method, r.Host, r.URL.RequestURI(), hex.EncodeToString(digest[:])}
	for _, header := range []string{"X-OhMyCine-Server-ID", HTTPTimeHeader, HTTPNonceHeader, "Range", "If-Match", "X-OhMyCine-Task-ID", "X-OhMyCine-Operation-Key", "Content-Type"} {
		fields = append(fields, header, r.Header.Get(header))
	}
	return []byte(strings.Join(fields, "\n")), nil
}
