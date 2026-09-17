# syntax=docker/dockerfile:1.7
FROM --platform=$BUILDPLATFORM node:22-bookworm-slim AS webui
WORKDIR /src/webui
COPY webui/package*.json ./
RUN npm ci
COPY webui/ ./
COPY internal/authz/catalog.json /src/internal/authz/catalog.json
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.23-bookworm AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG NODE_RELEASE_PUBLIC_KEY_B64=
WORKDIR /src
COPY go.mod go.sum ./
COPY webui/go.mod webui/go.sum ./webui/
RUN go mod download
COPY --from=webui /src/webui/dist ./webui/dist
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -tags webui -trimpath \
  -ldflags "-s -w -X=github.com/yuanjing-hash/OhMyCine-Server/internal/buildinfo.Version=${VERSION} -X=github.com/yuanjing-hash/OhMyCine-Server/internal/buildinfo.Commit=${COMMIT} -X=github.com/yuanjing-hash/OhMyCine-Server/internal/buildinfo.NodeReleasePublicKeyBase64=${NODE_RELEASE_PUBLIC_KEY_B64}" \
  -o /out/ohmycine-server ./cmd/server

FROM --platform=$BUILDPLATFORM node:22-bookworm-slim AS browser-companion
WORKDIR /opt/ohmycine/browser-companion
COPY browser-companion/package*.json ./
# Never execute upstream installers or acquire a browser during image builds.
RUN npm ci --omit=dev --ignore-scripts
COPY browser-companion/src ./src

FROM node:22-bookworm-slim AS runtime
COPY LICENSE /usr/share/doc/ohmycine/LICENSE
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata ffmpeg \
    libasound2 libatk-bridge2.0-0 libatk1.0-0 libatspi2.0-0 libcups2 libdbus-1-3 \
    libdrm2 libgbm1 libnspr4 libnss3 libx11-6 libxcb1 libxcomposite1 libxdamage1 \
    libxext6 libxfixes3 libxkbcommon0 libxrandr2 libpango-1.0-0 libcairo2 \
    fonts-liberation fonts-noto-cjk fonts-noto-color-emoji \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /var/lib/ohmycine/data /var/lib/ohmycine/logs /var/lib/ohmycine/plugins /var/lib/ohmycine/browser \
    && chmod 700 /var/lib/ohmycine/browser \
    && chown -R 65532:65532 /var/lib/ohmycine
COPY --from=browser-companion /opt/ohmycine/browser-companion /opt/ohmycine/browser-companion
ENV OMC_ENV=production OMC_SERVER_HOST=0.0.0.0 OMC_SERVER_PORT=3000 OMC_DATABASE_PATH=/var/lib/ohmycine/data/ohmycine.db OMC_LOG_DIR=/var/lib/ohmycine/logs OMC_PLUGIN_DIR=/var/lib/ohmycine/plugins
ENV OMC_CLOAK_NODE=/usr/local/bin/node OMC_CLOAK_COMPANION=/opt/ohmycine/browser-companion/src/main.mjs OMC_CLOAK_DATA_DIR=/var/lib/ohmycine/browser
VOLUME ["/var/lib/ohmycine"]
EXPOSE 3000
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/ohmycine-server"]

# CI uses the exact verified Release binaries, preserving TMDB and Node trust roots.
FROM runtime AS release
ARG TARGETARCH
COPY --chmod=755 build/container-release/${TARGETARCH}/ohmycine-server /usr/local/bin/ohmycine-server

FROM runtime AS local
COPY --from=build /out/ohmycine-server /usr/local/bin/ohmycine-server
