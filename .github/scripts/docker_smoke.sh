#!/usr/bin/env bash
# Runs only on an ephemeral GitHub runner, with empty named volumes.
set -euo pipefail
: "${IMAGE_REGISTRIES:?}" "${VERSION:?}"
prefix="omc-smoke-${GITHUB_RUN_ID:?}-${GITHUB_RUN_ATTEMPT:?}"
cleanup() {
  docker rm -f "${prefix}-server" "${prefix}-node" >/dev/null 2>&1 || true
  docker volume rm "${prefix}-server" "${prefix}-node" >/dev/null 2>&1 || true
}
trap cleanup EXIT
IFS=',' read -ra registries <<< "$IMAGE_REGISTRIES"
for registry in "${registries[@]}"; do
for arch in amd64 arm64; do
  for component in server node; do
    image="${registry}/ohmycine-${component}:v${VERSION}"
    docker pull --platform "linux/${arch}" "$image"
    [[ "$(docker image inspect --format '{{.Architecture}}' "$image")" == "$arch" ]]
  done
  server="${registry}/ohmycine-server:v${VERSION}"
  node="${registry}/ohmycine-node:v${VERSION}"
  docker run -d --platform "linux/${arch}" --name "${prefix}-server" \
    -p 127.0.0.1:13000:3000 -v "${prefix}-server:/var/lib/ohmycine" "$server"
  # Use the normal HTTPS transport, with a generated disposable enrollment token.
  token="$(openssl rand -hex 32)"
  docker run -d --platform "linux/${arch}" --name "${prefix}-node" \
    -e OMC_NODE_ID=ci-smoke -e "OMC_NODE_ENROLLMENT_TOKEN=$token" \
    -p 127.0.0.1:14433:4433 -v "${prefix}-node:/var/lib/ohmycine-node" "$node"
  for endpoint in http://127.0.0.1:13000/api/v1/health https://127.0.0.1:14433/node/v1/health; do
    # Self-signed Node identity is expected before pairing. Nothing leaves loopback.
    curl --insecure --fail --silent --show-error --retry 30 --retry-delay 2 \
      --retry-all-errors --max-time 5 "$endpoint" >/dev/null
  done
  docker exec "${prefix}-server" test -s /var/lib/ohmycine/data/credentials.key
  # Validate the managed wrapper, not licensed browser acquisition/launch.
  docker exec "${prefix}-server" /usr/local/bin/node -e 'if(Number(process.versions.node.split(".")[0])<20) process.exit(1)'
  docker exec "${prefix}-server" test -s /opt/ohmycine/browser-companion/src/main.mjs
  docker exec "${prefix}-server" test -s /opt/ohmycine/browser-companion/node_modules/cloakbrowser/package.json
  docker exec "${prefix}-server" test -w /var/lib/ohmycine/browser
  docker exec "${prefix}-node" test -s /var/lib/ohmycine-node/node.key
  docker restart "${prefix}-server" "${prefix}-node" >/dev/null
  curl --fail --silent --show-error --retry 30 --retry-delay 2 --retry-all-errors \
    --max-time 5 http://127.0.0.1:13000/api/v1/health >/dev/null
  curl --insecure --fail --silent --show-error --retry 30 --retry-delay 2 --retry-all-errors \
    --max-time 5 https://127.0.0.1:14433/node/v1/health >/dev/null
  cleanup
done
done
