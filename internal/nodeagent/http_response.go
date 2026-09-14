package nodeagent

import (
	"bytes"
	"crypto"
	"crypto/tls"
	"errors"
	"net/http"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

type signedResponseBuffer struct {
	header   http.Header
	body     bytes.Buffer
	status   int
	overflow bool
}

func (b *signedResponseBuffer) Header() http.Header { return b.header }
func (b *signedResponseBuffer) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}
func (b *signedResponseBuffer) Write(data []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	if b.body.Len()+len(data) > nodeprotocol.MaxHTTPResponseBytes {
		b.overflow = true
		return 0, errors.New("node_response_too_large")
	}
	return b.body.Write(data)
}

func (a *Agent) signedHTTPHandler(next http.Handler, w http.ResponseWriter, r *http.Request) {
	pair, err := tls.LoadX509KeyPair(a.config.TLSCertificateFile, a.config.TLSPrivateKeyFile)
	if err != nil || len(pair.Certificate) == 0 {
		http.Error(w, "node_signing_identity_invalid", 500)
		return
	}
	key, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		http.Error(w, "node_signing_identity_invalid", 500)
		return
	}
	buffer := &signedResponseBuffer{header: w.Header().Clone()}
	next.ServeHTTP(buffer, r)
	if buffer.overflow {
		http.Error(w, "node_response_too_large", 500)
		return
	}
	if buffer.status == 0 {
		buffer.status = http.StatusOK
	}
	if err := nodeprotocol.SignHTTPResponse(r, buffer.status, buffer.header, buffer.body.Bytes(), pair.Certificate[0], key); err != nil {
		http.Error(w, "node_response_signing_failed", 500)
		return
	}
	for name, values := range buffer.header {
		w.Header()[name] = values
	}
	w.WriteHeader(buffer.status)
	_, _ = w.Write(buffer.body.Bytes())
}
