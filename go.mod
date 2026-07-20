// PROVISIONAL module identity: the final module path is decided by name clearance
// (see docs/naming.md). The rename is one atomic change across five surfaces (module
// path, proto package, csharp_namespace, image name, chart name) and must complete
// before the first persistent deployment.
module github.com/OCluster/opencluster-relay

go 1.26

require (
	google.golang.org/grpc v1.70.0
	google.golang.org/protobuf v1.36.5
)

require (
	golang.org/x/net v0.32.0 // indirect
	golang.org/x/sys v0.28.0 // indirect
	golang.org/x/text v0.21.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20241202173237-19429a94021a // indirect
)
