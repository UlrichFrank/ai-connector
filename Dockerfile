# syntax=docker/dockerfile:1

# The server (cmd/ai-connector-server, see docs/architecture.md): a pure-Go binary with the React web app embedded,
# for the Raspberry Pi k0s cluster (image pharndt/ai-connector-server). It also carries the worker program
# (cmd/ai-connector for macOS, Linux and Windows) of the same build, served at /tools/mlxlab/<platform> for
# "Connect a worker".
#
#   docker buildx build --platform linux/arm64 -t pharndt/ai-connector-server:dev -f Dockerfile --load .   (make image)
#
# The build stages run on the build machine's platform and cross-compile (CGO_ENABLED=0), so an arm64 image
# builds natively fast on a Mac or an amd64 CI runner without emulation; the runtime stage has no RUN steps.

# ---- web app (web/ → internal/webui/dist) ----
FROM --platform=$BUILDPLATFORM node:24-alpine AS web
ENV COREPACK_ENABLE_DOWNLOAD_PROMPT=0
RUN corepack enable && corepack prepare pnpm@11.4.0 --activate
WORKDIR /src/web
COPY web/package.json web/pnpm-lock.yaml ./
RUN --mount=type=cache,target=/root/.local/share/pnpm/store \
    pnpm install --frozen-lockfile
COPY web/ ./
# vite writes to ../internal/webui/dist (vite.config.ts)
RUN mkdir -p ../internal/webui/dist && pnpm build

# ---- the console (console/ → internal/consoleui/dist) ----
FROM --platform=$BUILDPLATFORM node:24-alpine AS console
ENV COREPACK_ENABLE_DOWNLOAD_PROMPT=0
RUN corepack enable && corepack prepare pnpm@11.4.0 --activate
WORKDIR /src/console
COPY console/package.json console/pnpm-lock.yaml ./
RUN --mount=type=cache,target=/root/.local/share/pnpm/store \
    pnpm install --frozen-lockfile
COPY console/ ./
# vite writes to ../internal/consoleui/dist (vite.config.ts)
RUN mkdir -p ../internal/consoleui/dist && pnpm build

# ---- Go sources with the web app and the console (both binaries embed them) ----
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS src
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY sandbox/ sandbox/
COPY --from=web /src/internal/webui/dist/ internal/webui/dist/
COPY --from=console /src/internal/consoleui/dist/ internal/consoleui/dist/

# ---- server binary ----
FROM src AS hub
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/ai-connector-server ./cmd/ai-connector-server \
    && mkdir -p /out/data

# ---- the worker program for every platform (cmd/ai-connector), see internal/hub/tools.go ----
# Without cgo: on a Mac MLX is reached through purego and the embedded mlx-c dylib (the Go linker signs it ad hoc);
# on Linux and Windows the worker runs the embedder, the classifier and local OpenAI-compatible runtimes.
FROM src AS workers
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    for p in darwin-arm64 linux-amd64 linux-arm64 windows-amd64; do \
      CGO_ENABLED=0 GOOS=${p%-*} GOARCH=${p#*-} \
      go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/tools/mlxlab/$p ./cmd/ai-connector \
      && chmod 0755 /out/tools/mlxlab/$p || exit 1; \
    done

# ---- runtime ----
# distroless static: CA certificates, tzdata, /etc/passwd with nonroot (65532), no shell.
# There is no curl/wget for a Docker HEALTHCHECK; Kubernetes probes GET /api/info (no auth) instead.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=hub /out/ai-connector-server /usr/local/bin/ai-connector-server
# the worker programs, readable by anyone (root-owned, 0755), served by the server at /tools/mlxlab/<platform>
COPY --from=workers /out/tools/ /tools/
# empty, nonroot-owned data dir (devices.json, workers.json) so fresh volumes are writable
COPY --from=hub --chown=65532:65532 /out/data /data
ENV MLXLAB_DATA=/data MLXLAB_ADDR=:8080 MLXLAB_TOOLS=/tools
VOLUME /data
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/ai-connector-server"]
