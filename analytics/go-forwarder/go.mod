module github.com/jonathaneoliver/infinite-streaming/analytics/go-forwarder

go 1.25.0

require (
	github.com/google/uuid v1.6.0
	github.com/jonathaneoliver/infinite-streaming/go-proxy v0.0.0-00010101000000-000000000000
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/gorilla/mux v1.8.1 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/oapi-codegen/runtime v1.4.0 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
)

// Local sibling module — the workspace's go.work also covers this for
// `go build`, but the explicit replace lets `go mod tidy` resolve the
// import without hitting GOPROXY.
replace github.com/jonathaneoliver/infinite-streaming/go-proxy => ../../go-proxy
