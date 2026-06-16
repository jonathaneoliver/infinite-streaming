module github.com/jonathaneoliver/infinite-streaming/tools/harness-cli

go 1.25.0

require (
	github.com/google/uuid v1.6.0
	github.com/oapi-codegen/runtime v1.4.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/jonathaneoliver/infinite-streaming/go-proxy v0.0.0-00010101000000-000000000000
)

replace github.com/jonathaneoliver/infinite-streaming/go-proxy => ../../go-proxy
