module github.com/whitaker-io/machine/lang/lint

go 1.27

require (
	github.com/whitaker-io/machine/lang/analysis v0.0.0
	github.com/whitaker-io/machine/lang/ast v0.0.0
)

replace github.com/whitaker-io/machine/lang/analysis => ../analysis

replace github.com/whitaker-io/machine/lang/ast => ../ast
