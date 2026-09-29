package services

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/buildinfo"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

type TransferNodeInstallation struct {
	Available          bool   `json:"available"`
	Shell              string `json:"shell,omitempty"`
	Command            string `json:"command,omitempty"`
	Version            string `json:"version,omitempty"`
	DockerHubNamespace string `json:"docker_hub_namespace,omitempty"`
}

var (
	nodeInstallerSHA256Pattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	nodeDockerHubNamespacePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,127}$`)
)

func (s *TransferNodeService) Installation(nodeID, token string) (TransferNodeInstallation, error) {
	var node models.TransferNode
	if err := s.db.First(&node, "id = ?", strings.TrimSpace(nodeID)).Error; err != nil {
		return TransferNodeInstallation{}, transferNodeNotFound(err)
	}
	installation := transferNodeInstallation(node, token)
	return installation, validateTransferNodeInstallation(installation)
}

func transferNodeInstallation(node models.TransferNode, token string) TransferNodeInstallation {
	info := buildinfo.Current()
	publicKey := strings.TrimSpace(buildinfo.NodeReleasePublicKeyBase64)
	shellSHA256 := strings.TrimSpace(buildinfo.NodeInstallerShellSHA256)
	powerShellSHA256 := strings.TrimSpace(buildinfo.NodeInstallerPowerShellSHA256)
	dockerHubNamespace := strings.TrimSpace(buildinfo.NodeDockerHubNamespace)
	token = strings.TrimSpace(token)
	if !info.Official || !validNodeInstallationIdentity(node.ID, token) {
		return TransferNodeInstallation{}
	}
	if !validNodeReleasePublicKey(publicKey) ||
		!nodeInstallerSHA256Pattern.MatchString(shellSHA256) ||
		!nodeInstallerSHA256Pattern.MatchString(powerShellSHA256) ||
		!nodeDockerHubNamespacePattern.MatchString(dockerHubNamespace) {
		return TransferNodeInstallation{}
	}
	parsed, err := url.Parse(node.APIURL)
	if err != nil {
		return TransferNodeInstallation{}
	}
	transport := strings.ToLower(parsed.Scheme)
	if transport != "http" && transport != "https" {
		return TransferNodeInstallation{}
	}
	port := parsed.Port()
	if port == "" {
		if transport == "http" {
			port = "80"
		} else {
			port = "443"
		}
	}
	parsedPort, err := strconv.ParseUint(port, 10, 16)
	if err != nil || parsedPort == 0 {
		return TransferNodeInstallation{}
	}
	switch node.Platform {
	case "linux":
		return TransferNodeInstallation{
			Available: true, Shell: "bash", Version: info.Version, DockerHubNamespace: dockerHubNamespace,
			Command: linuxNodeBootstrap(info.Version, node.ID, token, port, shellSHA256, transport),
		}
	case "windows":
		return TransferNodeInstallation{
			Available: true, Shell: "powershell", Version: info.Version, DockerHubNamespace: dockerHubNamespace,
			Command: windowsNodeBootstrap(info.Version, node.ID, token, port, powerShellSHA256, transport),
		}
	default:
		return TransferNodeInstallation{}
	}
}

func validNodeInstallationIdentity(nodeID, token string) bool {
	if _, err := uuid.Parse(strings.TrimSpace(nodeID)); err != nil {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(token))
	return err == nil && len(decoded) == 32
}

func validNodeReleasePublicKey(value string) bool {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return false
	}
	block, rest := pem.Decode(raw)
	if block == nil || block.Type != "PUBLIC KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return false
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	key, ok := parsed.(*rsa.PublicKey)
	return err == nil && ok && key.N.BitLen() >= 3072 && key.E >= 3
}

func linuxNodeBootstrap(version, nodeID, token, port, installerSHA256, transport string) string {
	return fmt.Sprintf(`( set -e; tmp="$(mktemp)"; trap 'rm -f -- "$tmp"' EXIT; curl --fail --location --proto '=https' --proto-redir '=https' --tlsv1.2 'https://github.com/yuanjing-hash/OhMyCine-Server/releases/download/server-v%[1]s/install-ohmycine-node-v%[1]s.sh' -o "$tmp"; printf '%%s  %%s\n' '%[5]s' "$tmp" | sha256sum --check --status; sudo bash "$tmp" --version '%[1]s' --node-id '%[2]s' --enrollment-token '%[3]s' --listen '0.0.0.0:%[4]s' --transport '%[6]s' )`, version, nodeID, token, port, installerSHA256, transport)
}

func windowsNodeBootstrap(version, nodeID, token, port, installerSHA256, transport string) string {
	return fmt.Sprintf(`$ErrorActionPreference='Stop'; $tmp=Join-Path ([IO.Path]::GetTempPath()) ('ohmycine-node-'+[guid]::NewGuid().ToString('N')+'.ps1'); try { Invoke-WebRequest -UseBasicParsing -Uri 'https://github.com/yuanjing-hash/OhMyCine-Server/releases/download/server-v%[1]s/install-ohmycine-node-v%[1]s.ps1' -OutFile $tmp; if ((Get-FileHash -Algorithm SHA256 -LiteralPath $tmp).Hash.ToLowerInvariant() -ne '%[5]s') { throw 'OhMyCine Node installer checksum mismatch' }; & pwsh -NoLogo -NoProfile -ExecutionPolicy Bypass -File $tmp -Version '%[1]s' -NodeId '%[2]s' -EnrollmentToken '%[3]s' -ListenAddress '0.0.0.0:%[4]s' -Transport '%[6]s'; if ($LASTEXITCODE -ne 0) { throw "OhMyCine Node installer exited with $LASTEXITCODE" } } finally { if (Test-Path -LiteralPath $tmp) { Remove-Item -LiteralPath $tmp -Force } }`, version, nodeID, token, port, installerSHA256, transport)
}

func validateTransferNodeInstallation(installation TransferNodeInstallation) error {
	if !installation.Available {
		return nil
	}
	if installation.Command == "" || installation.Shell != "bash" && installation.Shell != "powershell" {
		return errors.New("transfer node installation command is invalid")
	}
	return nil
}
