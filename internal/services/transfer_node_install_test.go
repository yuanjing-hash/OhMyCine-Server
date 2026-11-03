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
	restoreNodeInstallationBuildInfo(t)
	node := models.TransferNode{ID: uuid.NewString(), APIURL: "https://node.example.com:4433", Platform: "linux"}
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))

	buildinfo.Version, buildinfo.Commit, buildinfo.NodeReleasePublicKeyBase64 = "dev", "unknown", ""
	buildinfo.NodeInstallerShellSHA256 = strings.Repeat("a", 64)
	buildinfo.NodeInstallerPowerShellSHA256 = strings.Repeat("b", 64)
	buildinfo.NodeDockerHubNamespace = "laomohouzi"
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
	buildinfo.NodeReleasePublicKeyBase64 = testNodeReleasePublicKey(t, 3072)
	for _, test := range []struct {
		name string
		set  func()
	}{
		{name: "missing shell digest", set: func() { buildinfo.NodeInstallerShellSHA256 = "" }},
		{name: "invalid PowerShell digest", set: func() { buildinfo.NodeInstallerPowerShellSHA256 = strings.Repeat("z", 64) }},
		{name: "unsafe Docker namespace", set: func() { buildinfo.NodeDockerHubNamespace = "owner/other" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			buildinfo.NodeInstallerShellSHA256 = strings.Repeat("a", 64)
			buildinfo.NodeInstallerPowerShellSHA256 = strings.Repeat("b", 64)
			buildinfo.NodeDockerHubNamespace = "laomohouzi"
			test.set()
			if got := transferNodeInstallation(node, token); got.Available || got.Command != "" {
				t.Fatalf("untrusted release metadata was accepted: %+v", got)
			}
		})
	}
}

func TestTransferNodeInstallationRejectsUntrustedCommandInputs(t *testing.T) {
	restoreNodeInstallationBuildInfo(t)
	buildinfo.Version = "1.2.3"
	buildinfo.Commit = strings.Repeat("c", 40)
	buildinfo.NodeReleasePublicKeyBase64 = testNodeReleasePublicKey(t, 3072)
	buildinfo.NodeInstallerShellSHA256 = strings.Repeat("a", 64)
	buildinfo.NodeInstallerPowerShellSHA256 = strings.Repeat("b", 64)
	buildinfo.NodeDockerHubNamespace = "laomohouzi"
	validToken := base64.RawURLEncoding.EncodeToString(make([]byte, 32))

	for _, test := range []struct {
		name  string
		node  models.TransferNode
		token string
	}{
		{name: "injected node ID", node: models.TransferNode{ID: "node'; touch owned; #", APIURL: "https://node.example.com", Platform: "linux"}, token: validToken},
		{name: "injected token", node: models.TransferNode{ID: uuid.NewString(), APIURL: "https://node.example.com", Platform: "linux"}, token: "token'; touch owned; #"},
		{name: "invalid port", node: models.TransferNode{ID: uuid.NewString(), APIURL: "https://node.example.com:70000", Platform: "linux"}, token: validToken},
		{name: "zero port", node: models.TransferNode{ID: uuid.NewString(), APIURL: "https://node.example.com:0", Platform: "linux"}, token: validToken},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := transferNodeInstallation(test.node, test.token); got.Available || got.Command != "" {
				t.Fatalf("unsafe command input was accepted: %+v", got)
			}
		})
	}
}

func restoreNodeInstallationBuildInfo(t *testing.T) {
	t.Helper()
	version, commit, key := buildinfo.Version, buildinfo.Commit, buildinfo.NodeReleasePublicKeyBase64
	shellSHA, powerShellSHA, namespace := buildinfo.NodeInstallerShellSHA256, buildinfo.NodeInstallerPowerShellSHA256, buildinfo.NodeDockerHubNamespace
	t.Cleanup(func() {
		buildinfo.Version, buildinfo.Commit, buildinfo.NodeReleasePublicKeyBase64 = version, commit, key
		buildinfo.NodeInstallerShellSHA256, buildinfo.NodeInstallerPowerShellSHA256, buildinfo.NodeDockerHubNamespace = shellSHA, powerShellSHA, namespace
	})
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
