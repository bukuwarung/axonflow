module github.com/getaxonflow/axonflow/examples/integrations/agentgateway-adapter

go 1.25

// Versions pinned to match platform/go.mod (known-good with the AxonFlow build).
// The Envoy ext_authz v3 protos live in the split-out go-control-plane/envoy
// module; go mod tidy resolves the transitive google.golang.org/genproto/rpc
// status module that the CheckResponse status field depends on.
require (
	github.com/envoyproxy/go-control-plane/envoy v1.36.0
	google.golang.org/grpc v1.80.0
	google.golang.org/protobuf v1.36.11 // indirect
)

require google.golang.org/genproto/googleapis/rpc v0.0.0-20260120221211-b8f7ae30c516

require (
	github.com/cncf/xds/go v0.0.0-20251210132809-ee656c7534f5 // indirect
	github.com/envoyproxy/protoc-gen-validate v1.3.0 // indirect
	github.com/planetscale/vtprotobuf v0.6.1-0.20240319094008-0393e58bdf10 // indirect
	golang.org/x/net v0.49.0 // indirect
	golang.org/x/sys v0.40.0 // indirect
	golang.org/x/text v0.33.0 // indirect
)
