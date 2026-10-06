FROM golang:1.27.1-bookworm AS go-build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/ocpool ./cmd/ocpool \
    && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/opencode-probe ./cmd/opencode-probe

FROM node:24-bookworm AS node-deps
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --omit=dev --ignore-scripts \
    && npm install --omit=dev --no-save opencode-ai@1.18.34
ENV CLOAKBROWSER_CACHE_DIR=/opt/cloakbrowser
RUN mkdir -p /opt/cloakbrowser /app/.home \
    && node --input-type=module -e "import('cloakbrowser').then(({ensureBinary}) => ensureBinary())"

FROM node:24-bookworm
ENV DEBIAN_FRONTEND=noninteractive \
    NODE_ENV=production \
    HOME=/app/.home \
    OPENCODE_BINARY=/app/node_modules/.bin/opencode \
    CLOAKBROWSER_CACHE_DIR=/opt/cloakbrowser \
    CLOAKBROWSER_AUTO_UPDATE=false \
    OCPool_ROOT=/app
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl dumb-init fonts-liberation xdg-utils \
      libasound2 libcairo2 libcups2 libdbus-1-3 libdrm2 libgbm1 libglib2.0-0 \
      libgtk-3-0 libnspr4 libnss3 libpango-1.0-0 libx11-6 libx11-xcb1 libxcb1 \
      libxcomposite1 libxdamage1 libxext6 libxfixes3 libxkbcommon0 libxrandr2 \
      libatspi2.0-0 \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=go-build /out/ocpool /app/bin/ocpool
COPY --from=go-build /out/opencode-probe /app/bin/opencode-probe
COPY --from=node-deps /app/node_modules /app/node_modules
COPY --from=node-deps /opt/cloakbrowser /opt/cloakbrowser
COPY scripts /app/scripts
COPY email-worker /app/email-worker
COPY package.json package-lock.json /app/
COPY docker/app-entrypoint.sh docker/configure-compose.mjs /usr/local/bin/
RUN mkdir -p /app/.state /app/.home && chmod 0700 /app/.state /app/.home /opt/cloakbrowser \
    && chmod 0755 /app/bin/ocpool /app/bin/opencode-probe /usr/local/bin/app-entrypoint.sh /usr/local/bin/configure-compose.mjs
EXPOSE 10000
HEALTHCHECK --interval=10s --timeout=3s --start-period=20s --retries=6 CMD curl -fsS http://127.0.0.1:10000/health || exit 1
ENTRYPOINT ["/usr/bin/dumb-init", "--", "/usr/local/bin/app-entrypoint.sh"]
