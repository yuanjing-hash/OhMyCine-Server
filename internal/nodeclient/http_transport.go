package nodeclient

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

type signedTransport struct {
	base            http.RoundTripper
	identity        Identity
	nodeFingerprint string
}

func (t *signedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "http" {
		return nil, errors.New("node_transport_mismatch")
	}
	key, ok := t.identity.Certificate.PrivateKey.(ed25519.PrivateKey)
	if !ok || len(t.identity.Certificate.Certificate) != 1 {
		return nil, errors.New("node_controller_identity_invalid")
	}
	if err := nodeprotocol.SignHTTPRequest(req, t.identity.Certificate.Certificate[0], key, t.nodeFingerprint, time.Now().UTC()); err != nil {
		return nil, err
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, nodeprotocol.MaxHTTPResponseBytes+1))
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if len(body) > nodeprotocol.MaxHTTPResponseBytes {
		return nil, errors.New("node_response_too_large")
	}
	if err := nodeprotocol.VerifyHTTPResponse(req, resp, body, t.nodeFingerprint); err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}
