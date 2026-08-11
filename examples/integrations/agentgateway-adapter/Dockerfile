# syntax=docker/dockerfile:1.7
# Multi-stage build for both gateway PEPs: the ext_authz adapter (default
# entrypoint, :9090) and the ExtMcp ext_proc shim (override the entrypoint
# with /usr/local/bin/axonflow-mcp-shim, :9091).
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" \
      -o /out/axonflow-gateway-adapters ./cmd/axonflow-gateway-adapters && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" \
      -o /out/axonflow-mcp-shim ./cmd/axonflow-mcp-shim

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/axonflow-gateway-adapters /usr/local/bin/axonflow-gateway-adapters
COPY --from=build /out/axonflow-mcp-shim /usr/local/bin/axonflow-mcp-shim
EXPOSE 9090
EXPOSE 9091
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/axonflow-gateway-adapters"]
