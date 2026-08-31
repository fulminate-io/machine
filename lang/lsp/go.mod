module github.com/whitaker-io/machine/lang/lsp

go 1.27

require (
	github.com/whitaker-io/machine/lang/analysis v0.0.0-00010101000000-000000000000
	github.com/whitaker-io/machine/lang/ast v0.0.0
	go.lsp.dev/jsonrpc2 v1.0.1
	go.lsp.dev/protocol v1.0.1
	go.lsp.dev/uri v1.0.1
)

require github.com/go-json-experiment/json v0.0.0-20260623181947-01eb4420fa68 // indirect

replace github.com/whitaker-io/machine/lang/analysis => ../analysis

replace github.com/whitaker-io/machine/lang/ast => ../ast
