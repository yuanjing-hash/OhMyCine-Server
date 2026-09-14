package nodeclient

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

type Identity struct {
	ServerID, CertificatePEM, PrivateKeyPEM, Fingerprint string
	Certificate                                          tls.Certificate
	ExpiresAt                                            time.Time
}

func GenerateIdentity(serverID string, now time.Time) (Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Identity{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return Identity{}, err
	}
	template := x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "OhMyCine Server Node Controller"}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(5, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, pub, priv)
	if err != nil {
		return Identity{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return Identity{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	return ParseIdentity(serverID, string(certPEM), string(keyPEM))
}

func ParseIdentity(serverID, certPEM, keyPEM string) (Identity, error) {
	pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil || len(pair.Certificate) == 0 {
		return Identity{}, errors.New("node_controller_identity_invalid")
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return Identity{}, errors.New("node_controller_identity_invalid")
	}
	sum := sha256.Sum256(pair.Certificate[0])
	return Identity{ServerID: serverID, CertificatePEM: certPEM, PrivateKeyPEM: keyPEM, Fingerprint: hex.EncodeToString(sum[:]), Certificate: pair, ExpiresAt: cert.NotAfter}, nil
}

type Client struct {
	baseURL, nodeFingerprint string
	identity                 Identity
	http                     *http.Client
}

type EnrollmentIdentity struct {
	CertificateFingerprint string
	EncryptionPublicKey    string
}

func New(baseURL, nodeFingerprint string, identity Identity) (*Client, error) {
	if strings.TrimSpace(identity.ServerID) == "" || !nodeprotocol.ValidDigest(nodeFingerprint) {
		return nil, errors.New("node_client_identity_invalid")
	}
	normalized, err := NormalizePublicURL(baseURL)
	if err != nil {
		return nil, err
	}
	transport := publicTransport()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, Certificates: []tls.Certificate{identity.Certificate}, VerifyConnection: func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) != 1 {
			return errors.New("node_certificate_missing")
		}
		sum := sha256.Sum256(state.PeerCertificates[0].Raw)
		if !nodeprotocol.SecureEqualHex(hex.EncodeToString(sum[:]), nodeFingerprint) {
			return errors.New("node_certificate_mismatch")
		}
		return nil
	}}
	client := boundedHTTPClient(transport)
	if strings.HasPrefix(normalized, "http://") {
		client.Transport = &signedTransport{base: transport, identity: identity, nodeFingerprint: nodeFingerprint}
	}
	return &Client{baseURL: normalized, nodeFingerprint: nodeFingerprint, identity: identity, http: client}, nil
}

func Enroll(ctx context.Context, baseURL, nodeID, token string, identity Identity) (EnrollmentIdentity, error) {
	return enrollWithTransport(ctx, baseURL, nodeID, token, identity, publicTransport())
}

func enrollWithTransport(ctx context.Context, baseURL, nodeID, token string, identity Identity, transport *http.Transport) (EnrollmentIdentity, error) {
	normalized, err := NormalizePublicURL(baseURL)
	if err != nil {
		return EnrollmentIdentity{}, err
	}
	var observed string
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, Certificates: []tls.Certificate{identity.Certificate}, VerifyConnection: func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) != 1 {
			return errors.New("node_certificate_missing")
		}
		sum := sha256.Sum256(state.PeerCertificates[0].Raw)
		observed = hex.EncodeToString(sum[:])
		return nil
	}}
	defer transport.CloseIdleConnections()
	client := boundedHTTPClient(transport)
	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		return EnrollmentIdentity{}, err
	}
	nonce := hex.EncodeToString(nonceBytes)
	challengeInput := nodeprotocol.EnrollmentChallengeRequest{NodeID: nodeID, ServerID: identity.ServerID, Nonce: nonce}
	var challenge nodeprotocol.EnrollmentChallengeResponse
	if err := doJSON(ctx, client, http.MethodPost, normalized+"/node/v1/enrollment/challenge", token, challengeInput, &challenge); err != nil {
		return EnrollmentIdentity{}, err
	}
	if strings.HasPrefix(normalized, "http://") {
		observed = challenge.NodeCertificateFingerprint
	}
	if observed == "" || challenge.NodeID != nodeID || challenge.ServerID != identity.ServerID || challenge.Nonce != nonce || !nodeprotocol.SecureEqualHex(observed, challenge.NodeCertificateFingerprint) || !validX25519PublicKey(challenge.NodeEncryptionPublicKey) {
		return EnrollmentIdentity{}, errors.New("node_enrollment_identity_mismatch")
	}
	want := nodeprotocol.EnrollmentProof([]byte(token), "node", nodeID, identity.ServerID, nonce, observed, challenge.NodeEncryptionPublicKey, "")
	if !nodeprotocol.SecureEqualHex(want, challenge.NodeProof) {
		return EnrollmentIdentity{}, errors.New("node_enrollment_proof_invalid")
	}
	complete := nodeprotocol.EnrollmentCompleteRequest{NodeID: nodeID, ServerID: identity.ServerID, Nonce: nonce, NodeCertificateFingerprint: observed, NodeEncryptionPublicKey: challenge.NodeEncryptionPublicKey, ServerCertificateFingerprint: identity.Fingerprint, ServerProof: nodeprotocol.EnrollmentProof([]byte(token), "server", nodeID, identity.ServerID, nonce, observed, challenge.NodeEncryptionPublicKey, identity.Fingerprint)}
	var result nodeprotocol.EnrollmentCompleteResponse
	if err := doJSON(ctx, client, http.MethodPost, normalized+"/node/v1/enrollment/complete", token, complete, &result); err != nil {
		return EnrollmentIdentity{}, err
	}
	if !result.Paired || result.NodeID != nodeID || result.ServerID != identity.ServerID {
		return EnrollmentIdentity{}, errors.New("node_enrollment_incomplete")
	}
	if strings.HasPrefix(normalized, "http://") && !nodeprotocol.SecureEqualHex(result.NodeProof, nodeprotocol.EnrollmentProof([]byte(token), "complete", nodeID, identity.ServerID, nonce, observed, challenge.NodeEncryptionPublicKey, identity.Fingerprint)) {
		return EnrollmentIdentity{}, errors.New("node_enrollment_proof_invalid")
	}
	return EnrollmentIdentity{CertificateFingerprint: observed, EncryptionPublicKey: challenge.NodeEncryptionPublicKey}, nil
}

func (c *Client) Health(ctx context.Context) (nodeprotocol.HealthResponse, error) {
	var out nodeprotocol.HealthResponse
	err := c.do(ctx, http.MethodGet, "/node/v1/health", nil, &out)
	return out, err
}
func (c *Client) PutOperation(ctx context.Context, input nodeprotocol.PutOperationRequest) (nodeprotocol.OperationResponse, error) {
	var out nodeprotocol.OperationResponse
	err := c.do(ctx, http.MethodPut, "/node/v1/operations/"+input.Plan.OperationKey, input, &out)
	return out, err
}
func (c *Client) Operation(ctx context.Context, key string) (nodeprotocol.OperationResponse, error) {
	var out nodeprotocol.OperationResponse
	err := c.do(ctx, http.MethodGet, "/node/v1/operations/"+key, nil, &out)
	return out, err
}
func (c *Client) RenewOperationLease(ctx context.Context, key string, input nodeprotocol.LeaseRequest) (nodeprotocol.OperationResponse, error) {
	var out nodeprotocol.OperationResponse
	err := c.do(ctx, http.MethodPost, "/node/v1/operations/"+key+"/lease", input, &out)
	return out, err
}
func (c *Client) CancelOperation(ctx context.Context, key string) (nodeprotocol.OperationResponse, error) {
	var out nodeprotocol.OperationResponse
	err := c.do(ctx, http.MethodPost, "/node/v1/operations/"+key+"/cancel", nil, &out)
	return out, err
}
func (c *Client) PutCredentialGrant(ctx context.Context, envelope nodeprotocol.CredentialGrantEnvelope) error {
	return c.do(ctx, http.MethodPut, "/node/v1/credential-grants/"+envelope.GrantID, envelope, nil)
}
func (c *Client) DownloaderAction(ctx context.Context, input nodeprotocol.DownloaderActionRequest) (nodeprotocol.DownloaderActionResponse, error) {
	var out nodeprotocol.DownloaderActionResponse
	err := c.do(ctx, http.MethodPost, "/node/v1/downloader/actions", input, &out)
	return out, err
}
func (c *Client) StorageAction(ctx context.Context, input nodeprotocol.StorageActionRequest) (nodeprotocol.StorageActionResponse, error) {
	var out nodeprotocol.StorageActionResponse
	err := c.do(ctx, http.MethodPost, "/node/v1/storage/actions", input, &out)
	return out, err
}
func (c *Client) StorageActionResult(ctx context.Context, requestID, taskID, operationKey string) (nodeprotocol.StorageActionResponse, error) {
	var out nodeprotocol.StorageActionResponse
	headers := http.Header{}
	headers.Set("X-OhMyCine-Task-ID", taskID)
	headers.Set("X-OhMyCine-Operation-Key", operationKey)
	err := c.doWithHeaders(ctx, http.MethodGet, "/node/v1/storage/actions/"+requestID, nil, &out, headers)
	return out, err
}

func (c *Client) StorageSourceAction(ctx context.Context, input nodeprotocol.StorageSourceActionRequest) (nodeprotocol.StorageSourceActionResponse, error) {
	var out nodeprotocol.StorageSourceActionResponse
	err := c.do(ctx, http.MethodPost, "/node/v1/storage-source/actions", input, &out)
	return out, err
}

func (c *Client) StorageSourceActionResult(ctx context.Context, requestID, taskID, operationKey string) (nodeprotocol.StorageSourceActionResponse, error) {
	var out nodeprotocol.StorageSourceActionResponse
	headers := http.Header{}
	headers.Set("X-OhMyCine-Task-ID", taskID)
	headers.Set("X-OhMyCine-Operation-Key", operationKey)
	err := c.doWithHeaders(ctx, http.MethodGet, "/node/v1/storage-source/actions/"+requestID, nil, &out, headers)
	return out, err
}

func (c *Client) CleanupStorageSource(ctx context.Context, input nodeprotocol.StorageSourceCleanupRequest) (nodeprotocol.StorageSourceCleanupResponse, error) {
	var out nodeprotocol.StorageSourceCleanupResponse
	err := c.do(ctx, http.MethodPost, "/node/v1/operations/"+input.OperationKey+"/cleanup", input, &out)
	return out, err
}

type RemoteError struct {
	Code string
}

func (e *RemoteError) Error() string { return e.Code }

func (c *Client) do(ctx context.Context, method, path string, input, output any) error {
	return c.doWithHeaders(ctx, method, path, input, output, nil)
}

func (c *Client) doWithHeaders(ctx context.Context, method, path string, input, output any, headers http.Header) error {
	var body *bytes.Reader
	if input == nil {
		body = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-OhMyCine-Server-ID", c.identity.ServerID)
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var remote nodeprotocol.ErrorResponse
		if decodeBoundedJSON(resp.Body, &remote) == nil && strings.TrimSpace(remote.Code) != "" {
			return &RemoteError{Code: remote.Code}
		}
		return &RemoteError{Code: "node_request_failed"}
	}
	if output == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return decodeBoundedJSON(resp.Body, output)
}
func doJSON(ctx context.Context, client *http.Client, method, target, token string, input, output any) error {
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if req.URL.Scheme == "http" {
		req.Header.Set("X-OhMyCine-Enrollment-Proof", nodeprotocol.HTTPEnrollmentProof(token, input))
	} else {
		req.Header.Set("X-OhMyCine-Enrollment-Token", token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New("node_enrollment_request_failed")
	}
	return decodeBoundedJSON(resp.Body, output)
}

func boundedHTTPClient(transport *http.Transport) *http.Client {
	return &http.Client{
		Transport: transport,
		Timeout:   15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func publicTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, errors.New("node_address_invalid")
			}
			addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, errors.New("node_dns_unavailable")
			}
			for _, candidate := range addresses {
				if !isPublicIP(candidate.IP) {
					continue
				}
				connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
				if dialErr == nil {
					return connection, nil
				}
			}
			return nil, errors.New("node_public_address_unreachable")
		},
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   4,
	}
}

// NormalizePublicURL validates an HTTP(S) public Node origin. HTTP remains
// authenticated and signed, but callers receive no transport encryption.
func NormalizePublicURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" && parsed.Path != "/" {
		return "", errors.New("node_public_url_invalid")
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return "", errors.New("node_public_url_required")
	}
	if ip := net.ParseIP(host); ip != nil && !isPublicIP(ip) {
		return "", errors.New("node_public_url_required")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func isPublicIP(ip net.IP) bool {
	return ip != nil && !ip.IsUnspecified() && !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast()
}

func validX25519PublicKey(value string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(raw) == 32
}

func decodeBoundedJSON(body io.Reader, output any) error {
	raw, err := io.ReadAll(io.LimitReader(body, nodeprotocol.MaxWireBodyBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > nodeprotocol.MaxWireBodyBytes {
		return errors.New("node_response_too_large")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("node_response_invalid: trailing content")
	}
	return nil
}
