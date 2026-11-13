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
# The final stage has no shell, so make /data here and copy it over.
RUN install -d -m 0700 /out/rootfs/data

# gcr.io/distroless/static-debian13:nonroot (ca-certificates, tzdata, no shell)
FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
LABEL org.opencontainers.image.source=https://github.com/bithyte/mcp-dock
COPY --from=build /out/mcp-dock /usr/local/bin/mcp-dock
COPY --from=build --chown=65532:65532 /out/rootfs/ /
ENV MCP_DOCK_CONFIG=/etc/mcp-dock/config.yaml
USER 65532:65532
ENTRYPOINT ["mcp-dock"]
CMD ["serve"]
