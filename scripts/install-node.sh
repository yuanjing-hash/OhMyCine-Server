#!/usr/bin/env bash
set -euo pipefail

# This source template is rendered by the official Server Beta workflow. The
# public key and version are intentionally unusable in a source checkout.
RELEASE_PUBLIC_KEY_B64='__OHMYCINE_NODE_RELEASE_PUBLIC_KEY_BASE64__'
DEFAULT_VERSION='__OHMYCINE_NODE_RELEASE_VERSION__'
OFFICIAL_RELEASE_ROOT='https://github.com/yuanjing-hash/OhMyCine-Server/releases/download'

VERSION="$DEFAULT_VERSION"
NODE_ID=''
ENROLLMENT_TOKEN=''
LISTEN_ADDRESS='0.0.0.0:4433'
TRANSPORT='https'
TLS_CERT=''
TLS_KEY=''
CLIENT_CA=''
DATA_DIR='/var/lib/ohmycine-node'
MANAGED_ROOT='/var/lib/ohmycine-node/managed'
SERVICE_USER='root'
VERIFY_ONLY=false
VERIFY_MANIFEST=''
VERIFY_SIGNATURE=''
VERIFY_ARCHIVE=''
VERIFY_ASSET=''

usage() {
  cat <<'EOF'
Install the official OhMyCine transfer Node as a systemd service.

Required:
  --node-id ID --enrollment-token TOKEN

Optional:
  --version X.Y.Z              Version baked into this installer by default
  --listen HOST:PORT           Default: 0.0.0.0:4433
  --transport http|https      Default: https. HTTP remains authenticated but is not encrypted.
  --tls-cert PATH --tls-key PATH
                                Optional explicit certificate pair. When both
                                are omitted, a pinned self-signed pair is
                                generated under DATA_DIR/tls.
  --client-ca PATH             Optional Server client CA bundle
  --data-dir PATH              Default: /var/lib/ohmycine-node
  --managed-root PATH          Default: DATA_DIR/managed
  --service-user USER          Existing account, default: root

This installer only installs OhMyCine Node. It does not install a downloader,
cloud-drive client, reverse proxy, or firewall rule.
EOF
}

while (($#)); do
  case "$1" in
    --version) VERSION="${2:-}"; shift 2 ;;
    --node-id) NODE_ID="${2:-}"; shift 2 ;;
    --enrollment-token) ENROLLMENT_TOKEN="${2:-}"; shift 2 ;;
    --listen) LISTEN_ADDRESS="${2:-}"; shift 2 ;;
    --transport) TRANSPORT="${2:-}"; shift 2 ;;
    --tls-cert) TLS_CERT="${2:-}"; shift 2 ;;
    --tls-key) TLS_KEY="${2:-}"; shift 2 ;;
    --client-ca) CLIENT_CA="${2:-}"; shift 2 ;;
    --data-dir) DATA_DIR="${2:-}"; shift 2 ;;
    --managed-root) MANAGED_ROOT="${2:-}"; shift 2 ;;
    --service-user) SERVICE_USER="${2:-}"; shift 2 ;;
    --verify-only) VERIFY_ONLY=true; shift ;;
    --manifest) VERIFY_MANIFEST="${2:-}"; shift 2 ;;
    --signature) VERIFY_SIGNATURE="${2:-}"; shift 2 ;;
    --archive) VERIFY_ARCHIVE="${2:-}"; shift 2 ;;
    --asset-name) VERIFY_ASSET="${2:-}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

fail() { echo "OhMyCine Node installation failed: $1" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "$1 is required but was not found"; }

if [[ "$RELEASE_PUBLIC_KEY_B64" == __OHMYCINE_* || "$DEFAULT_VERSION" == __OHMYCINE_* ]]; then
  fail 'this source template is not a signed release installer'
fi
[[ "$VERSION" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || fail 'version must be X.Y.Z'
need openssl
need sha256sum

TMP_ROOT="$(mktemp -d)"
cleanup() { rm -rf -- "$TMP_ROOT"; }
trap cleanup EXIT
PUBLIC_KEY_FILE="$TMP_ROOT/node-release-public.pem"
printf '%s' "$RELEASE_PUBLIC_KEY_B64" | base64 --decode > "$PUBLIC_KEY_FILE" 2>/dev/null || fail 'embedded release public key is invalid'
openssl pkey -pubin -in "$PUBLIC_KEY_FILE" -noout >/dev/null 2>&1 || fail 'embedded release public key is invalid'

verify_release_payload() {
  local manifest="$1" signature="$2" archive="$3" asset="$4"
  [[ -f "$manifest" && -f "$signature" && -f "$archive" ]] || fail 'verification input is missing'
  case "$asset" in
    "OhMyCine-Node-v${VERSION}-linux-amd64.tar.gz"|"OhMyCine-Node-v${VERSION}-linux-arm64.tar.gz"|"OhMyCine-Node-v${VERSION}-windows-amd64.zip") ;;
    *) fail 'asset name does not match the selected Node version/platform' ;;
  esac
  (( $(wc -c < "$manifest") <= 1048576 )) || fail 'checksum manifest is too large'
  (( $(wc -c < "$signature") <= 65536 )) || fail 'manifest signature is too large'

  # The signature is the trust decision. Do not parse or use a checksum until
  # this succeeds.
  openssl dgst -sha256 -verify "$PUBLIC_KEY_FILE" -signature "$signature" "$manifest" >/dev/null 2>&1 || fail 'release manifest signature verification failed'

  local matches expected
  matches="$(awk -v asset="$asset" '$2 == asset && length($1) == 64 && $1 !~ /[^0-9a-f]/ { count++; value=$1 } END { print count+0, value }' "$manifest")"
  [[ "${matches%% *}" == '1' ]] || fail 'release manifest must contain exactly one checksum for this asset'
  expected="${matches#* }"
  [[ "$expected" =~ ^[0-9a-f]{64}$ ]] || fail 'release checksum is invalid'
  printf '%s  %s\n' "$expected" "$archive" | sha256sum --check --status || fail 'Node archive checksum verification failed'
}

if [[ "$VERIFY_ONLY" == true ]]; then
  [[ -n "$VERIFY_MANIFEST" && -n "$VERIFY_SIGNATURE" && -n "$VERIFY_ARCHIVE" && -n "$VERIFY_ASSET" ]] || fail 'verify-only requires manifest, signature, archive and asset-name'
  verify_release_payload "$VERIFY_MANIFEST" "$VERIFY_SIGNATURE" "$VERIFY_ARCHIVE" "$VERIFY_ASSET"
  echo 'OhMyCine Node release payload verified.'
  exit 0
fi

[[ "$(uname -s)" == 'Linux' ]] || fail 'this installer supports Linux only'
[[ "${EUID:-$(id -u)}" == '0' ]] || fail 'run this installer as root'
[[ "$NODE_ID" =~ ^[A-Za-z0-9._:-]{1,128}$ ]] || fail 'node ID is invalid'
[[ "$ENROLLMENT_TOKEN" =~ ^[A-Za-z0-9._~-]{20,512}$ ]] || fail 'enrollment token is invalid or truncated'
[[ "$LISTEN_ADDRESS" =~ ^[^[:space:]]+:[0-9]{1,5}$ ]] || fail 'listen address must be HOST:PORT'
[[ "$TRANSPORT" == 'http' || "$TRANSPORT" == 'https' ]] || fail 'transport must be http or https'
LISTEN_HOST="${LISTEN_ADDRESS%:*}"
PORT="${LISTEN_ADDRESS##*:}"
(( 10#$PORT >= 1 && 10#$PORT <= 65535 )) || fail 'listen port must be between 1 and 65535'
if [[ "$LISTEN_HOST" =~ ^\[([0-9A-Fa-f:]+)\]$ ]]; then
  CERT_LISTEN_SAN="IP:${BASH_REMATCH[1]}"
elif [[ "$LISTEN_HOST" =~ ^[0-9]+(\.[0-9]+){3}$ ]]; then
  IFS='.' read -r octet1 octet2 octet3 octet4 <<< "$LISTEN_HOST"
  for octet in "$octet1" "$octet2" "$octet3" "$octet4"; do
    (( 10#$octet <= 255 )) || fail 'listen IPv4 address is invalid'
  done
  CERT_LISTEN_SAN="IP:${LISTEN_HOST}"
elif [[ "$LISTEN_HOST" =~ ^[A-Za-z0-9.-]+$ ]]; then
  CERT_LISTEN_SAN="DNS:${LISTEN_HOST}"
else
  fail 'listen host is invalid'
fi
[[ "$DATA_DIR" == /* && "$MANAGED_ROOT" == /* ]] || fail 'data and managed paths must be absolute'
[[ "$DATA_DIR" != *'/../'* && "$DATA_DIR" != */.. && "$MANAGED_ROOT" != *'/../'* && "$MANAGED_ROOT" != */.. ]] || fail 'data and managed paths must not contain parent traversal'
DATA_CHECK="${DATA_DIR%/}"; [[ -n "$DATA_CHECK" ]] || DATA_CHECK='/'
MANAGED_CHECK="${MANAGED_ROOT%/}"; [[ -n "$MANAGED_CHECK" ]] || MANAGED_CHECK='/'
for unsafe_root in / /etc /usr /var /opt /home /root; do
  [[ "$DATA_CHECK" != "$unsafe_root" && "$MANAGED_CHECK" != "$unsafe_root" ]] || fail 'data and managed paths must not be broad system directories'
done
if [[ -n "$TLS_CERT" || -n "$TLS_KEY" ]]; then
  [[ -n "$TLS_CERT" && -n "$TLS_KEY" ]] || fail 'TLS certificate and key must be provided together'
  [[ "$TLS_CERT" == /* && -f "$TLS_CERT" && ! -L "$TLS_CERT" ]] || fail 'TLS certificate must be an existing absolute regular file'
  [[ "$TLS_KEY" == /* && -f "$TLS_KEY" && ! -L "$TLS_KEY" ]] || fail 'TLS private key must be an existing absolute regular file'
fi
if [[ -n "$CLIENT_CA" ]]; then
  [[ "$CLIENT_CA" == /* && -f "$CLIENT_CA" && ! -L "$CLIENT_CA" ]] || fail 'client CA must be an existing absolute regular file'
fi
id "$SERVICE_USER" >/dev/null 2>&1 || fail 'service user does not exist'
need curl
need tar
need systemctl
need install

case "$(uname -m)" in
  x86_64|amd64) ARCH='amd64' ;;
  aarch64|arm64) ARCH='arm64' ;;
  *) fail 'unsupported Linux architecture' ;;
esac
ASSET="OhMyCine-Node-v${VERSION}-linux-${ARCH}.tar.gz"
MANIFEST="OhMyCine-Node-v${VERSION}-SHA256SUMS.txt"
SIGNATURE="${MANIFEST}.sig"
RELEASE_URL="${OFFICIAL_RELEASE_ROOT}/server-v${VERSION}"

download_official() {
  local name="$1" target="$2" limit="$3" effective
  effective="$(curl --fail --silent --show-error --location --max-redirs 5 --proto '=https' --proto-redir '=https' --output "$target" --write-out '%{url_effective}' "${RELEASE_URL}/${name}")" || fail "could not download official release asset ${name}"
  case "$effective" in
    https://github.com/yuanjing-hash/OhMyCine-Server/releases/download/*|https://release-assets.githubusercontent.com/*|https://objects.githubusercontent.com/*) ;;
    *) fail 'release download redirected outside the official GitHub hosts' ;;
  esac
  (( $(wc -c < "$target") <= limit )) || fail "downloaded ${name} exceeds its size limit"
}

ARCHIVE_PATH="$TMP_ROOT/$ASSET"
MANIFEST_PATH="$TMP_ROOT/$MANIFEST"
SIGNATURE_PATH="$TMP_ROOT/$SIGNATURE"
download_official "$MANIFEST" "$MANIFEST_PATH" 1048576
download_official "$SIGNATURE" "$SIGNATURE_PATH" 65536
download_official "$ASSET" "$ARCHIVE_PATH" 2147483648
verify_release_payload "$MANIFEST_PATH" "$SIGNATURE_PATH" "$ARCHIVE_PATH" "$ASSET"

while IFS= read -r entry; do
  normalized="${entry#./}"
  [[ -n "$normalized" && "$normalized" != /* && "$normalized" != ../* && "$normalized" != *'/../'* ]] || fail 'Node archive contains an unsafe path'
done < <(tar -tzf "$ARCHIVE_PATH")
if tar -tvzf "$ARCHIVE_PATH" | awk 'substr($1,1,1) == "l" || substr($1,1,1) == "h" { found=1 } END { exit found ? 0 : 1 }'; then
  fail 'Node archive contains a link'
fi
EXTRACT_ROOT="$TMP_ROOT/extracted"
mkdir -p "$EXTRACT_ROOT"
tar -xzf "$ARCHIVE_PATH" -C "$EXTRACT_ROOT" --no-same-owner --no-same-permissions
SOURCE_BINARY="$EXTRACT_ROOT/OhMyCine-Node-v${VERSION}-linux-${ARCH}/ohmycine-node"
[[ -f "$SOURCE_BINARY" && ! -L "$SOURCE_BINARY" ]] || fail 'verified archive does not contain the expected Node binary'

for service_path in "$DATA_DIR" "$MANAGED_ROOT"; do
  if [[ -e "$service_path" ]]; then
    [[ -d "$service_path" && ! -L "$service_path" ]] || fail 'data and managed paths must be ordinary directories'
  else
    install -d -m 0700 -o "$SERVICE_USER" -g "$(id -gn "$SERVICE_USER")" "$service_path"
  fi
done
data_mode="$(stat -c '%a' "$DATA_DIR")"
(( (8#$data_mode & 8#077) == 0 )) || fail 'Node data directory permissions must not grant group or other access'
if [[ -z "$TLS_CERT" && -z "$TLS_KEY" ]]; then
  TLS_DIR="$DATA_DIR/tls"
  TLS_CERT="$TLS_DIR/node.crt"
  TLS_KEY="$TLS_DIR/node.key"
  if [[ -e "$TLS_CERT" || -e "$TLS_KEY" ]]; then
    [[ -f "$TLS_CERT" && ! -L "$TLS_CERT" && -f "$TLS_KEY" && ! -L "$TLS_KEY" ]] || fail 'existing generated TLS identity is incomplete or unsafe'
    chown "$SERVICE_USER:$(id -gn "$SERVICE_USER")" "$TLS_CERT" "$TLS_KEY"
    chmod 0644 "$TLS_CERT"
    chmod 0600 "$TLS_KEY"
  else
    install -d -m 0700 -o "$SERVICE_USER" -g "$(id -gn "$SERVICE_USER")" "$TLS_DIR"
    generated_cert="$TMP_ROOT/generated-node.crt"
    generated_key="$TMP_ROOT/generated-node.key"
    openssl req -x509 -newkey rsa:3072 -sha256 -nodes -days 825 \
      -subj "/CN=OhMyCine Node ${NODE_ID}" \
      -addext "subjectAltName=DNS:localhost,IP:127.0.0.1,IP:::1,${CERT_LISTEN_SAN}" \
      -keyout "$generated_key" -out "$generated_cert" >/dev/null 2>&1 || fail 'could not generate the Node TLS identity'
    install -m 0644 -o "$SERVICE_USER" -g "$(id -gn "$SERVICE_USER")" "$generated_cert" "$TLS_CERT"
    install -m 0600 -o "$SERVICE_USER" -g "$(id -gn "$SERVICE_USER")" "$generated_key" "$TLS_KEY"
  fi
fi

SERVICE_NAME='ohmycine-node.service'
WAS_ACTIVE=false
if systemctl is-active --quiet "$SERVICE_NAME"; then
  WAS_ACTIVE=true
  systemctl stop "$SERVICE_NAME"
fi
if command -v ss >/dev/null 2>&1 && ss -H -ltn "sport = :${PORT}" 2>/dev/null | grep -q .; then
  [[ "$WAS_ACTIVE" == true ]] && systemctl start "$SERVICE_NAME" || true
  fail "listen port ${PORT} is already in use"
fi

INSTALL_ROOT='/opt/ohmycine-node'
VERSION_ROOT="${INSTALL_ROOT}/versions/v${VERSION}"
CURRENT_LINK="${INSTALL_ROOT}/current"
PREVIOUS_TARGET=''
[[ -L "$CURRENT_LINK" ]] && PREVIOUS_TARGET="$(readlink "$CURRENT_LINK")"
install -d -m 0755 "$VERSION_ROOT"
install -m 0755 "$SOURCE_BINARY" "$VERSION_ROOT/ohmycine-node"
ln -sfn "$VERSION_ROOT" "$CURRENT_LINK"
if [[ "$SERVICE_USER" != root ]]; then
  need runuser
  runuser -u "$SERVICE_USER" -- test -w "$DATA_DIR" || fail 'service user cannot write the Node data directory'
  runuser -u "$SERVICE_USER" -- test -w "$MANAGED_ROOT" || fail 'service user cannot write the managed root'
  runuser -u "$SERVICE_USER" -- test -r "$TLS_CERT" || fail 'service user cannot read the TLS certificate'
  runuser -u "$SERVICE_USER" -- test -r "$TLS_KEY" || fail 'service user cannot read the TLS private key'
fi
install -d -m 0700 /etc/ohmycine-node
HAD_ENV=false
HAD_SERVICE_FILE=false
if [[ -e /etc/ohmycine-node/node.env || -L /etc/ohmycine-node/node.env ]]; then
  [[ -f /etc/ohmycine-node/node.env && ! -L /etc/ohmycine-node/node.env ]] || fail 'existing Node environment file is unsafe'
  cp -p /etc/ohmycine-node/node.env "$TMP_ROOT/previous-node.env"; HAD_ENV=true
fi
if [[ -e /etc/systemd/system/ohmycine-node.service || -L /etc/systemd/system/ohmycine-node.service ]]; then
  [[ -f /etc/systemd/system/ohmycine-node.service && ! -L /etc/systemd/system/ohmycine-node.service ]] || fail 'existing Node service file is unsafe'
  cp -p /etc/systemd/system/ohmycine-node.service "$TMP_ROOT/previous-ohmycine-node.service"; HAD_SERVICE_FILE=true
fi

escape_environment() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'; }
ENV_FILE="$TMP_ROOT/node.env"
{
  printf 'OMC_NODE_ID="%s"\n' "$(escape_environment "$NODE_ID")"
  printf 'OMC_NODE_LISTEN="%s"\n' "$(escape_environment "$LISTEN_ADDRESS")"
  printf 'OMC_NODE_TRANSPORT="%s"\n' "$(escape_environment "$TRANSPORT")"
  printf 'OMC_NODE_DATA_DIR="%s"\n' "$(escape_environment "$DATA_DIR")"
  printf 'OMC_NODE_MANAGED_ROOT="%s"\n' "$(escape_environment "$MANAGED_ROOT")"
  printf 'OMC_NODE_TLS_CERT="%s"\n' "$(escape_environment "$TLS_CERT")"
  printf 'OMC_NODE_TLS_KEY="%s"\n' "$(escape_environment "$TLS_KEY")"
  printf 'OMC_NODE_SEAL_KEY="%s"\n' "$(escape_environment "$DATA_DIR/node.seal.key")"
  printf 'OMC_NODE_ENROLLMENT_TOKEN="%s"\n' "$(escape_environment "$ENROLLMENT_TOKEN")"
  [[ -n "$CLIENT_CA" ]] && printf 'OMC_NODE_CLIENT_CA="%s"\n' "$(escape_environment "$CLIENT_CA")"
} > "$ENV_FILE"
install -m 0600 -o root -g root "$ENV_FILE" /etc/ohmycine-node/node.env

SERVICE_FILE="$TMP_ROOT/ohmycine-node.service"
cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=OhMyCine Transfer Node
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=$(id -gn "$SERVICE_USER")
EnvironmentFile=/etc/ohmycine-node/node.env
ExecStart=${CURRENT_LINK}/ohmycine-node
Restart=on-failure
RestartSec=5s
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=${DATA_DIR} ${MANAGED_ROOT}
RestrictSUIDSGID=true
LockPersonality=true

[Install]
WantedBy=multi-user.target
EOF
install -m 0644 -o root -g root "$SERVICE_FILE" /etc/systemd/system/ohmycine-node.service
systemctl daemon-reload
systemctl enable "$SERVICE_NAME" >/dev/null
if ! systemctl start "$SERVICE_NAME" || ! systemctl is-active --quiet "$SERVICE_NAME"; then
  systemctl stop "$SERVICE_NAME" >/dev/null 2>&1 || true
  if [[ -n "$PREVIOUS_TARGET" ]]; then
    ln -sfn "$PREVIOUS_TARGET" "$CURRENT_LINK"
  else
    rm -f -- "$CURRENT_LINK"
  fi
  if [[ "$HAD_ENV" == true ]]; then cp -p "$TMP_ROOT/previous-node.env" /etc/ohmycine-node/node.env; else rm -f -- /etc/ohmycine-node/node.env; fi
  if [[ "$HAD_SERVICE_FILE" == true ]]; then cp -p "$TMP_ROOT/previous-ohmycine-node.service" /etc/systemd/system/ohmycine-node.service; else rm -f -- /etc/systemd/system/ohmycine-node.service; fi
  systemctl daemon-reload >/dev/null 2>&1 || true
  [[ "$WAS_ACTIVE" == true ]] && systemctl start "$SERVICE_NAME" >/dev/null 2>&1 || true
  fail 'service did not start; the previous binary was restored when available (inspect journalctl -u ohmycine-node)'
fi

echo "OhMyCine Node v${VERSION} is installed and running. Complete pairing from the main Server."
