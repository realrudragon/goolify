# Multi-stage standalone Coolify build (Astro + Vue 3.5 + Go + Rust)
# Note: uses vue@^3.5.45 (latest compatible official Vue major on npm for @astrojs/vue@^7.1.6; Vue 4 is not published on npm).
# Production runtime contains NO PHP, Composer, Laravel, Livewire, Reverb, Node, or Svelte.

FROM node:24-alpine AS web-builder
WORKDIR /src/apps/web
COPY apps/web/package.json apps/web/package-lock.json* ./
RUN npm ci || npm install
COPY apps/web/ ./
RUN npm run check && npm run build

FROM rust:1.85-alpine AS rust-builder
RUN apk add --no-cache musl-dev
WORKDIR /src/executor
COPY executor/ ./
RUN cargo build --release --workspace

FROM golang:1.26-alpine AS go-builder
WORKDIR /src/backend
COPY backend/go.mod backend/go.sum* ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 go build -o /out/coolify-controlplane ./cmd/controlplane \
 && CGO_ENABLED=0 go build -o /out/coolify-worker ./cmd/worker \
 && CGO_ENABLED=0 go build -o /out/coolify-migrate ./cmd/migrate \
 && CGO_ENABLED=0 go build -o /out/coolify-cli ./cmd/coolify-cli

FROM alpine:3.21 AS runtime
RUN apk add --no-cache ca-certificates openssh-client docker-cli docker-cli-compose git curl bash tar gzip tzdata \
 && addgroup -g 1000 coolify \
 && adduser -D -u 1000 -G coolify coolify \
 && mkdir -p /data/coolify /app/web/dist \
 && chown -R coolify:coolify /data/coolify /app

WORKDIR /app
COPY --from=go-builder /out/coolify-controlplane /usr/local/bin/coolify-controlplane
COPY --from=go-builder /out/coolify-worker /usr/local/bin/coolify-worker
COPY --from=go-builder /out/coolify-migrate /usr/local/bin/coolify-migrate
COPY --from=go-builder /out/coolify-cli /usr/local/bin/coolify-cli
COPY --from=rust-builder /src/executor/target/release/coolify-executor /usr/local/bin/coolify-executor
COPY --from=web-builder /src/apps/web/dist /app/web/dist
COPY templates /app/templates
COPY svgs /app/svgs

USER coolify
EXPOSE 8000
HEALTHCHECK --interval=15s --timeout=5s --retries=3 CMD wget -qO- http://127.0.0.1:8000/api/v1/health || exit 1
ENTRYPOINT ["/usr/local/bin/coolify-controlplane"]
CMD ["-addr", ":8000", "-static-dir", "/app/web/dist", "-executor-bin", "/usr/local/bin/coolify-executor"]
