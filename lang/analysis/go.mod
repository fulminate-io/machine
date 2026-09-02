module github.com/whitaker-io/machine/lang/analysis

go 1.27

require (
	github.com/whitaker-io/machine/lang/ast v0.0.0
	github.com/whitaker-io/machine/lang/loader v0.0.0
)

require (
	golang.org/x/mod v0.39.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/tools v0.49.0 // indirect
)

replace github.com/whitaker-io/machine/lang/ast => ../ast

replace github.com/whitaker-io/machine/lang/loader => ../loader
