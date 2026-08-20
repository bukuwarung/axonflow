# syntax=docker/dockerfile:1.7
# Multi-stage build for the three gateway PEPs: the ext_authz adapter (default
# entrypoint, :9090), the ExtMcp ext_proc shim (entrypoint
# /usr/local/bin/axonflow-mcp-shim, :9091), and the LLM ext_proc shim
# (entrypoint /usr/local/bin/axonflow-llm-shim, :9092 — AID-100 Fix B).
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" \
      -o /out/axonflow-gateway-adapters ./cmd/axonflow-gateway-adapters && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" \
      -o /out/axonflow-mcp-shim ./cmd/axonflow-mcp-shim && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" \
      -o /out/axonflow-llm-shim ./cmd/axonflow-llm-shim

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/axonflow-gateway-adapters /usr/local/bin/axonflow-gateway-adapters
COPY --from=build /out/axonflow-mcp-shim /usr/local/bin/axonflow-mcp-shim
COPY --from=build /out/axonflow-llm-shim /usr/local/bin/axonflow-llm-shim
EXPOSE 9090
EXPOSE 9091
EXPOSE 9092
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/axonflow-gateway-adapters"]
