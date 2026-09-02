# The flow language

A `.flow` file declares one or more **flows**. A flow is a braceless list of
**statements**, each naming a node, its Go reference and the nodes it reads from.
The file is line-oriented: statements end at a newline, and every file ends with
one. Indentation is never significant; a line indented under a statement is a
**continuation** and is read as more of that statement's clauses.

## A whole file

```flow
import billing "github.com/acme/billing"
import "acme.dev/flows/audit"

const retries = 3
param endpoint string = ":8080"

flow orders
note """charges an order and records the outcome"""
state {
  charged int
  by_type map[string]int
}
var attempt int
var span *ops.Span clone ops.CloneSpan
on error audit.Alert
source ingest http.Listen[Order](endpoint)
transform charge billing.Charge from ingest
  reads attempt  writes charged
sink done audit.Store from charge
```

`import`, `const`, `param` and `func` are file-level declarations. `note`,
`state`, `var` and a flow-level `on error` belong to a flow. A `note` or an
`on error` must come **before the flow's first statement** to be read as the
flow's own: written after one, a line opening with `note` or `on` is that
statement's clause instead. `state` and `var` carry no such rule — the parser
accepts them anywhere in the body and attaches them to the flow wherever they
sit — but by convention they are written at the top, as every file here is.

A `func` is a declaration, not a node: it is legal anywhere at file level, it is
hoisted, and a statement reaches it by bare name.

```flow
func Enrich(f machine.Frame[Order]) Result { return Result{} }

flow orders
source ingest Poll
transform enrich Enrich from ingest
sink done audit.Store from enrich
```

## The ten statement forms

```flow
flow shapes
source ingest Poll
transform enrich Lookup from ingest
branch split Valid from enrich -> good, bad
tee fan from good -> ledger, mirror
sink store warehouse.Insert from ledger
loop retry
transform redo billing.Backoff from retry, mirror
send redo -> retry
drop bad
```

`source` opens a flow, `transform` maps a datum, `sink` ends a path. `branch`
takes exactly two targets and routes the intact datum down one; `tee` copies it
to every target it names. `loop` declares a re-entry point, and `send` is the
only backward arrow in the language: its target may be a loop label or a node
declared earlier. `drop` discards a path. `from` takes one or more upstream
names.

`switch` routes on a Go expression. It needs at least one arm; an `else` arm, if
present, must come last.

```flow
flow routing
source ingest Poll
switch route from ingest on ingest.Kind {
  "card", "wallet" -> billable
  isRefund(ingest) -> refundable
  else -> other
}
transform charge billing.Charge from billable
transform refund billing.Refund from refundable
sink hold audit.Store from other, charge, refund
```

## Signatures, and `use`

A flow's boundary is its **named outputs**. A signature declares the input type
and each output's name beside its Go type spelling; a flow without a signature is
typed from its body.

`use` embeds another flow. The identifiers after `->` **name** outputs of the
embedded flow: they are a set, order carries no meaning, and binding a subset is
legal. Naming something that is not one of that flow's outputs is an error, and
so is binding the same name twice.

```flow
flow screening (Order) -> ok OkResult, bad ErrResult
branch check fraud.Clean from in -> ok, bad

flow main
source ingest Poll
use screen screening from ingest -> ok, bad
sink store warehouse.Insert from ok
sink hold fraud.Quarantine from bad
```

A flow that declares a signature consumes an implicit `in`.

## Clauses

Every statement except the three bare forms — `drop`, `loop` and `send`, which
take no clauses at all — may carry `reads`, `writes`, `over`, `checkpoint`,
`idempotent`, `on error` and `note`. They are order-free, each may appear at most
once, and they may be written on the same line or on continuation lines.

`checkpoint` takes a Go-expression codec operand and journals the node.
`idempotent` is the one clause taking no operand, and it selects the **arrival**
anchor: the datum is journaled before the node runs, instead of the completion
default that journals what the node produced.

```flow
flow clauses
state {
  seen map[string]bool
}
var attempt int
source ingest Poll
transform a Step from ingest
  reads attempt  writes seen  over ratelimit.New(10)  idempotent  checkpoint machine.GobCodec[Order]{}
transform b Step from a
  note """clauses may be split across continuation lines"""
  on error Handle
sink done Store from b
```

A bare `checkpoint` is a syntax error, because a clause operand may not be empty:

```text
checkpoint                  ← refused: the codec operand is required
checkpoint machine.GobCodec[Order]{}
```

## Where a Go expression stops

A Go operand runs to a stop token, a clause keyword, a newline, or a `{`
**separated** from the expression before it. An **adjacent** `{` is part of the
expression, so `machine.GobCodec[Order]{}` stays whole while a switch body's
brace ends the span. That is what lets a clause carrying an operand sit last
before a switch body:

```flow
flow routing
source ingest Poll
switch route from ingest on ingest.Kind over ratelimit.New(5) {
  "card", "wallet" -> billable
  else -> other
}
switch audit from billable on billable.Kind checkpoint machine.GobCodec[Order]{} {
  "high" -> flagged
  else -> normal
}
sink hold audit.Store from other, flagged, normal
```

A func literal's body brace is written with a space before it, so a **bare** func
literal ends its span early and is refused. Parenthesize it:

```flow
flow escapes
source ingest Poll
transform a Step from ingest
  over (func() machine.Transport { return nil })
transform b Step from a
  checkpoint (func() machine.Codec[Order] { return nil })()
sink hold audit.Store from b
```

Braces inside a quoted string or a rune are text, never brackets, so
`over pubsub.Topic("a{b")` is a well-formed operand.
