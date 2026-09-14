package services

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/buildinfo"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestTransferNodeInstallationFailsClosedWithoutOfficialTrustRoot(t *testing.T) {
	oldVersion, oldCommit, oldKey := buildinfo.Version, buildinfo.Commit, buildinfo.NodeReleasePublicKeyBase64
	t.Cleanup(func() {
		buildinfo.Version, buildinfo.Commit, buildinfo.NodeReleasePublicKeyBase64 = oldVersion, oldCommit, oldKey
	})
	node := models.TransferNode{ID: uuid.NewString(), APIURL: "https://node.example.com:4433", Platform: "linux"}
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))

	buildinfo.Version, buildinfo.Commit, buildinfo.NodeReleasePublicKeyBase64 = "dev", "unknown", ""
	if got := transferNodeInstallation(node, token); got.Available || got.Command != "" || got.Shell != "" {
		t.Fatalf("development build exposed a pseudo-official installer: %+v", got)
	}
	buildinfo.Version, buildinfo.Commit, buildinfo.NodeReleasePublicKeyBase64 = "1.2.3", strings.Repeat("a", 40), base64.StdEncoding.EncodeToString([]byte("not a public key"))
	if got := transferNodeInstallation(node, token); got.Available {
		t.Fatalf("invalid release trust root was accepted: %+v", got)
	}
	buildinfo.NodeReleasePublicKeyBase64 = testNodeReleasePublicKey(t, 2048)
	if got := transferNodeInstallation(node, token); got.Available {
		t.Fatalf("undersized release trust root was accepted: %+v", got)
	}
}

func TestTransferNodeInstallationCommandsVerifyBeforeExecuting(t *testing.T) {
	oldVersion, oldCommit, oldKey := buildinfo.Version, buildinfo.Commit, buildinfo.NodeReleasePublicKeyBase64
	t.Cleanup(func() {
		buildinfo.Version, buildinfo.Commit, buildinfo.NodeReleasePublicKeyBase64 = oldVersion, oldCommit, oldKey
	})
	buildinfo.Version = "1.2.3"
	buildinfo.Commit = strings.Repeat("b", 40)
	buildinfo.NodeReleasePublicKeyBase64 = testNodeReleasePublicKey(t, 3072)
	nodeID := uuid.NewString()
	token := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

	linux := transferNodeInstallation(models.TransferNode{ID: nodeID, APIURL: "https://node.example.com:4433", Platform: "linux"}, token)
	if !linux.Available || linux.Shell != "bash" {
		t.Fatalf("linux installation unavailable: %+v", linux)
	}
	assertInstallerCommandOrder(t, linux.Command, "openssl dgst -sha256 -verify", "sha256sum", "sudo bash")
	for _, expected := range []string{"server-v1.2.3", "--node-id '" + nodeID + "'", "--enrollment-token '" + token + "'", "--listen '0.0.0.0:4433'"} {
		if !strings.Contains(linux.Command, expected) {
			t.Fatalf("linux command missing %q", expected)
		}
	}

	windows := transferNodeInstallation(models.TransferNode{ID: nodeID, APIURL: "https://node.example.com", Platform: "windows"}, token)
	if !windows.Available || windows.Shell != "powershell" {
		t.Fatalf("windows installation unavailable: %+v", windows)
	}
	assertInstallerCommandOrder(t, windows.Command, "$rsa.VerifyData", "Get-FileHash", "& pwsh")
	for _, expected := range []string{"server-v1.2.3", "-NodeId '" + nodeID + "'", "-EnrollmentToken '" + token + "'", "-ListenAddress '0.0.0.0:443'"} {
		if !strings.Contains(windows.Command, expected) {
			t.Fatalf("windows command missing %q", expected)
		}
	}
	for _, platform := range []string{"linux", "windows"} {
		for _, endpoint := range []string{"http://node.example.com", "http://node.example.com:8080"} {
			got := transferNodeInstallation(models.TransferNode{ID: nodeID, APIURL: endpoint, Platform: platform}, token)
			port := "80"
			if strings.HasSuffix(endpoint, ":8080") {
				port = "8080"
			}
			transport := "--transport 'http'"
			if platform == "windows" {
				transport = "-Transport 'http'"
			}
			if !got.Available || !strings.Contains(got.Command, transport) || !strings.Contains(got.Command, "0.0.0.0:"+port+"'") || strings.Contains(got.Command, "INSECURE_DEV") {
				t.Fatalf("HTTP installer has incorrect transport/port for %s %s", platform, endpoint)
			}
		}
	}
}

func TestTransferNodeInstallationRejectsUntrustedCommandInputs(t *testing.T) {
	oldVersion, oldCommit, oldKey := buildinfo.Version, buildinfo.Commit, buildinfo.NodeReleasePublicKeyBase64
	t.Cleanup(func() {
		buildinfo.Version, buildinfo.Commit, buildinfo.NodeReleasePublicKeyBase64 = oldVersion, oldCommit, oldKey
	})
	buildinfo.Version = "1.2.3"
	buildinfo.Commit = strings.Repeat("c", 40)
	buildinfo.NodeReleasePublicKeyBase64 = testNodeReleasePublicKey(t, 3072)
	validToken := base64.RawURLEncoding.EncodeToString(make([]byte, 32))

	for _, test := range []struct {
		name  string
		node  models.TransferNode
		token string
	}{
		{name: "injected node ID", node: models.TransferNode{ID: "node'; touch owned; #", APIURL: "https://node.example.com", Platform: "linux"}, token: validToken},
		{name: "injected token", node: models.TransferNode{ID: uuid.NewString(), APIURL: "https://node.example.com", Platform: "linux"}, token: "token'; touch owned; #"},
		{name: "invalid port", node: models.TransferNode{ID: uuid.NewString(), APIURL: "https://node.example.com:70000", Platform: "linux"}, token: validToken},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := transferNodeInstallation(test.node, test.token); got.Available || got.Command != "" {
				t.Fatalf("unsafe command input was accepted: %+v", got)
			}
		})
	}
}

func assertInstallerCommandOrder(t *testing.T, command string, stages ...string) {
	t.Helper()
	previous := -1
	for _, stage := range stages {
		index := strings.Index(command, stage)
		if index < 0 || index <= previous {
			t.Fatalf("installer stage %q is missing or out of order", stage)
		}
		previous = index
	}
}

func testNodeReleasePublicKey(t *testing.T, bits int) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	payload := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	return base64.StdEncoding.EncodeToString(payload)
}
