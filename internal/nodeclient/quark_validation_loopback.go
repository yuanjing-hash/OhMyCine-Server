//go:build quark_live

package nodeclient

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"
)

// This acceptance-only origin never uses DNS or a proxy. Production New and
// NormalizePublicURL retain their public-origin policy.
const QuarkValidationNodeOrigin = "http://quark-node-validation.example.com"

func quarkValidationTransport(socket string) (*http.Transport, error) {
	host, port, err := net.SplitHostPort(socket)
	n, portErr := strconv.Atoi(port)
	if err != nil || host != "127.0.0.1" || portErr != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return nil, errors.New("quark_node_validation_socket_invalid")
	}
	transport := publicTransport()
	transport.DisableKeepAlives = true
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "quark-node-validation.example.com:80" || network != "tcp" && network != "tcp4" {
			return nil, errors.New("quark_node_validation_origin_invalid")
		}
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp4", socket)
	}
	return transport, nil
}

func EnrollQuarkValidationNode(ctx context.Context, socket, nodeID, token string, identity Identity) (EnrollmentIdentity, error) {
	transport, err := quarkValidationTransport(socket)
	if err != nil {
		return EnrollmentIdentity{}, err
	}
	return enrollWithTransport(ctx, QuarkValidationNodeOrigin, nodeID, token, identity, transport)
}

func NewQuarkValidationNodeClient(socket, fingerprint string, identity Identity) (*Client, error) {
	transport, err := quarkValidationTransport(socket)
	if err != nil {
		return nil, err
	}
	client, err := New(QuarkValidationNodeOrigin, fingerprint, identity)
	if err != nil {
		return nil, err
	}
	client.http.Transport = &signedTransport{base: transport, identity: identity, nodeFingerprint: fingerprint}
	return client, nil
}
