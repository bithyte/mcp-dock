# syntax=docker/dockerfile:1
# Base refs are pinned to the multi-arch index digest, resolved 2026-11-12.

# golang:1.27.2-trixie
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27.2-trixie@sha256:47667934874913a449b96faa7ef4777fa285d9ec29126ee80ed61c45b7cccf0e AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -mod=readonly -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/mcp-dock ./cmd/mcp-dock

# ghcr.io/astral-sh/uv:0.13.7, only used to copy the uv binaries
FROM ghcr.io/astral-sh/uv:0.13.7@sha256:96f40d3904aad8d0bd010539001d36c8e2188b981e1334471a90c4a1d93bb490 AS uv

# node:24-trixie-slim
FROM docker.io/library/node:24-trixie-slim@sha256:af02d57b57719b9657a45199a79264a9437940cd55b3c7b3fc99615654f124a0
LABEL org.opencontainers.image.source=https://github.com/bithyte/mcp-dock
RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 libatomic1 ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=uv /uv /uvx /usr/local/bin/
COPY --from=build /out/mcp-dock /usr/local/bin/mcp-dock
COPY image/npmrc /usr/local/etc/npmrc
COPY image/uv.toml /etc/uv/uv.toml
ENV MCP_DOCK_CONFIG=/etc/mcp-dock/config.yaml HOME=/data/home
USER 65532:65532
ENTRYPOINT ["mcp-dock"]
CMD ["serve"]
