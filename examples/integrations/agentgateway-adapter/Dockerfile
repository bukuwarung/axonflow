# syntax=docker/dockerfile:1.7
# Multi-stage build for the ext_authz gRPC PEP.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" \
      -o /out/axonflow-gateway-adapters ./cmd/axonflow-gateway-adapters

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/axonflow-gateway-adapters /usr/local/bin/axonflow-gateway-adapters
EXPOSE 9090
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/axonflow-gateway-adapters"]
