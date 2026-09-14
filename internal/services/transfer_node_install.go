package services

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/buildinfo"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

type TransferNodeInstallation struct {
	Available bool   `json:"available"`
	Shell     string `json:"shell,omitempty"`
	Command   string `json:"command,omitempty"`
}

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
	token = strings.TrimSpace(token)
	if !info.Comparable || publicKey == "" || !validNodeInstallationIdentity(node.ID, token) {
		return TransferNodeInstallation{}
	}
	if !validNodeReleasePublicKey(publicKey) {
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
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return TransferNodeInstallation{}
	}
	switch node.Platform {
	case "linux":
		return TransferNodeInstallation{Available: true, Shell: "bash", Command: linuxNodeBootstrap(info.Version, node.ID, token, port, publicKey, transport)}
	case "windows":
		return TransferNodeInstallation{Available: true, Shell: "powershell", Command: windowsNodeBootstrap(info.Version, node.ID, token, port, publicKey, transport)}
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

func linuxNodeBootstrap(version, nodeID, token, port, publicKey, transport string) string {
	return fmt.Sprintf(`set -euo pipefail
tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT
root='https://github.com/yuanjing-hash/OhMyCine-Server/releases/download/server-v%[1]s'
manifest='OhMyCine-Node-v%[1]s-SHA256SUMS.txt'
signature="${manifest}.sig"
installer='install-ohmycine-node-v%[1]s.sh'
curl --fail --location --proto '=https' --tlsv1.2 "$root/$manifest" -o "$tmp/$manifest"
curl --fail --location --proto '=https' --tlsv1.2 "$root/$signature" -o "$tmp/$signature"
curl --fail --location --proto '=https' --tlsv1.2 "$root/$installer" -o "$tmp/$installer"
printf '%%s' '%[5]s' | base64 --decode > "$tmp/release-public.pem"
openssl dgst -sha256 -verify "$tmp/release-public.pem" -signature "$tmp/$signature" "$tmp/$manifest"
expected="$(awk -v name="$installer" '$2 == name { print $1 }' "$tmp/$manifest")"
[ "${#expected}" -eq 64 ] && [ "$(sha256sum "$tmp/$installer" | awk '{print $1}')" = "$expected" ]
sudo bash "$tmp/$installer" --version '%[1]s' --node-id '%[2]s' --enrollment-token '%[3]s' --listen '0.0.0.0:%[4]s' --transport '%[6]s'`, version, nodeID, token, port, publicKey, transport)
}

func windowsNodeBootstrap(version, nodeID, token, port, publicKey, transport string) string {
	return fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$tmp = Join-Path ([IO.Path]::GetTempPath()) ('ohmycine-node-' + [guid]::NewGuid().ToString('N'))
[IO.Directory]::CreateDirectory($tmp) | Out-Null
try {
  $root = 'https://github.com/yuanjing-hash/OhMyCine-Server/releases/download/server-v%[1]s'
  $manifest = 'OhMyCine-Node-v%[1]s-SHA256SUMS.txt'
  $signature = $manifest + '.sig'
  $installer = 'install-ohmycine-node-v%[1]s.ps1'
  Invoke-WebRequest -UseBasicParsing -Uri "$root/$manifest" -OutFile (Join-Path $tmp $manifest)
  Invoke-WebRequest -UseBasicParsing -Uri "$root/$signature" -OutFile (Join-Path $tmp $signature)
  Invoke-WebRequest -UseBasicParsing -Uri "$root/$installer" -OutFile (Join-Path $tmp $installer)
  $pem = [Text.Encoding]::ASCII.GetString([Convert]::FromBase64String('%[5]s'))
  $rsa = [Security.Cryptography.RSA]::Create()
  try {
    $rsa.ImportFromPem($pem)
    $valid = $rsa.VerifyData([IO.File]::ReadAllBytes((Join-Path $tmp $manifest)), [IO.File]::ReadAllBytes((Join-Path $tmp $signature)), [Security.Cryptography.HashAlgorithmName]::SHA256, [Security.Cryptography.RSASignaturePadding]::Pkcs1)
    if (-not $valid) { throw 'OhMyCine Node release signature is invalid' }
  } finally { $rsa.Dispose() }
  $escaped = [regex]::Escape($installer)
  $matches = @(Get-Content -LiteralPath (Join-Path $tmp $manifest) | Where-Object { $_ -match "^([0-9a-f]{64})  $escaped$" })
  if ($matches.Count -ne 1) { throw 'Installer checksum is missing or ambiguous' }
  [void]($matches[0] -match '^([0-9a-f]{64})')
  if ((Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $tmp $installer)).Hash.ToLowerInvariant() -ne $Matches[1]) { throw 'Installer checksum mismatch' }
  & pwsh -NoLogo -NoProfile -ExecutionPolicy Bypass -File (Join-Path $tmp $installer) -Version '%[1]s' -NodeId '%[2]s' -EnrollmentToken '%[3]s' -ListenAddress '0.0.0.0:%[4]s' -Transport '%[6]s'
  if ($LASTEXITCODE -ne 0) { throw "OhMyCine Node installer exited with $LASTEXITCODE" }
} finally {
  if (Test-Path -LiteralPath $tmp) { Remove-Item -LiteralPath $tmp -Recurse -Force }
}`, version, nodeID, token, port, publicKey, transport)
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
