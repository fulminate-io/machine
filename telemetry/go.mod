module github.com/whitaker-io/machine/telemetry

go 1.25.0

require (
	github.com/whitaker-io/machine/common v0.1.1
	go.opentelemetry.io/otel v1.46.0
	go.opentelemetry.io/otel/metric v1.46.0
	go.opentelemetry.io/otel/trace v1.46.0
)

require github.com/cespare/xxhash/v2 v2.3.0 // indirect
