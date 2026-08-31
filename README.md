[![Go](https://github.com/whitaker-io/machine/actions/workflows/go.yml/badge.svg)](https://github.com/whitaker-io/machine/actions/workflows/go.yml)
[![CodeQL](https://github.com/whitaker-io/machine/actions/workflows/github-code-scanning/codeql/badge.svg)](https://github.com/whitaker-io/machine/actions/workflows/github-code-scanning/codeql)
[![PkgGoDev](https://pkg.go.dev/badge/github.com/whitaker-io/machine/v4)](https://pkg.go.dev/github.com/whitaker-io/machine/v4)
[![Go Report Card](https://goreportcard.com/badge/github.com/whitaker-io/machine/v4)](https://goreportcard.com/report/github.com/whitaker-io/machine/v4)
[![Codacy Badge](https://app.codacy.com/project/badge/Grade/aa8efa7beb3f4e66a5dc0247e25557b5)](https://app.codacy.com/gh/whitaker-io/machine/dashboard?utm_source=gh&utm_medium=referral&utm_content=&utm_campaign=Badge_grade)
[![Codacy Badge](https://app.codacy.com/project/badge/Coverage/aa8efa7beb3f4e66a5dc0247e25557b5)](https://app.codacy.com/gh/whitaker-io/machine/dashboard?utm_source=gh&utm_medium=referral&utm_content=&utm_campaign=Badge_coverage)
[![Version Badge](https://img.shields.io/github/v/tag/whitaker-io/machine)](https://img.shields.io/github/v/tag/whitaker-io/machine)

`Machine` is a library for creating data workflows. These workflows can be either very concise or quite complex, even allowing for cycles for flows that need retry or self healing mechanisms.

------

### **Requirements**

Go **1.27** or newer. The builder is a fluent chain of **generic methods** — `Machine.Source[T]`, `Flow.Map[V]`, `Frame.Get[V]` — and methods carrying their own type parameters are a Go 1.27 language feature. There is no earlier-Go fallback.

### **Installation**

```bash
  go get github.com/whitaker-io/machine/v4
```

The transports live in their own modules, so a program that needs neither pulls in neither:

```bash
  go get github.com/whitaker-io/machine/edge/http
  go get github.com/whitaker-io/machine/edge/pubsub
```

------

### **Quick start**

A `Machine` is the supervisor. You declare a graph off it, then start it once.

```golang
ctx, cancel := context.WithCancel(context.Background())
defer cancel()

m := machine.New("pipeline")

numbers, send := m.Source[int]("numbers")
numbers.
	Map("double", func(f machine.Frame[int]) int { return f.Value() * 2 }).
	Map("label", func(f machine.Frame[int]) string { return fmt.Sprintf("n=%d", f.Value()) }).
	Drop("discard")

if err := m.Start(ctx); err != nil {
	log.Fatal(err)
}
if err := send(ctx, 21); err != nil {
	log.Fatal(err)
}
```

`Source` returns the flow **and** the `Ingest` closure that feeds it. `Map` changes the payload type, so the chain above goes `int` → `int` → `string`.

To consume results rather than discard them, end the chain with `Output`, which hands back the channel of packets leaving the flow:

```golang
out := numbers.
	Map("double", func(f machine.Frame[int]) int { return f.Value() * 2 }).
	Output("out")

// ... start and send ...

packet := <-out
fmt.Println(packet.Value(), packet.ID(), packet.Source(), packet.Node())
```

Packets reaching an `Output` belong to the **caller** and are not reclaimed. Everywhere else the runtime reclaims a datum's stack state when the traversal ends.

------

### **The model**

Three types carry the whole API.

| Type | What it is |
| --- | --- |
| `Machine` | The supervisor. Owns the node registry, the heap store, the telemetry handles and the declaration errors. Its exported method set is deliberately closed at `Host`, `Name`, `Source` and `Start`. |
| `Flow[T, U]` | A declared node's outbound handle — `T` is the source payload type, `U` the current one. It holds only the machine and the emitter, so it is cheap to pass by value. |
| `Frame[T]` | The **node-facing** envelope. It **wraps** the payload rather than traveling beside it, so a node function takes `Frame[T]` and returns a bare payload. |
| `Packet[T]` | The **edge-facing** envelope. It carries identity, lineage and the serializable projection of the datum's stack state, and no capability-gated accessor at all. |

A node function cannot drop the frame by ignoring it and cannot forge one — the runtime re-wraps the returned payload onto the same frame state. There is no exported frame constructor at all.

`Frame` and `Packet` are one state seen from two sides. The runtime converts a frame to a packet when it hands a datum to an edge, and mints a fresh frame carrying the **receiving** node's declared capability view when a packet arrives. That conversion is unexported, so a node cannot obtain a packet and an edge never sees a frame. For a channel edge the packet wraps the same state pointer and costs nothing; the projection is built only when a remote codec asks for it.

Declaration is lazy and `Start` does the real work: it validates the whole graph, joins **every** declaration error rather than reporting the first, brings up every edge, and only then spawns the nodes. A mis-declared graph is inert rather than half-running.

Shutdown **is** context cancellation. `Start` is the only lifecycle entry, so there is no `Stop`; when the context ends the runtime closes every constructed edge.

------

### **Building a graph**

Every builder method takes a node name. Names are unique per machine — a duplicate is a declaration error reported from `Start`.

| Method | Effect |
| --- | --- |
| `Map[V](name, fn, opts...)` | Applies `fn` to each frame and forwards the transformed payload, changing the flow's payload type. |
| `If(name, fn, opts...)` | Splits the flow in two, routing the **intact** frame down one branch. |
| `Tee(name, fn, opts...)` | Duplicates the flow, deep-copying the envelope into both branches. |
| `Send(target)` | Merges this flow into the same consumer `target` already feeds. This is how a cycle closes. |
| `Drop(name, opts...)` | Terminates the flow, discarding each frame and reclaiming its stack state. |
| `Output(name, opts...)` | Terminal consumption surface: returns `<-chan Packet[U]`. Does not process, so it does not advance the datum's `Node` stamp. |

#### Branching

`If` routes the intact frame: no copy and no reparenting, so identity and lineage survive the branch unchanged. The branches are named with a `.left` and `.right` suffix, so an unconsumed-branch error at `Start` identifies which side was left dangling.

```golang
large, small := src.If("over-ten", func(f machine.Frame[int]) bool { return f.Value() > 10 })

out := large.Output("large")
small.Drop("small")
```

`Tee` is the other split, and it deep-copies. You supply the payload duplicator, because only you know how to copy `T` without leaving the two branches sharing a pointer, slice or map; the runtime clones the frame. Both branches get fresh identities and both report the upstream frame as their parent.

```golang
audit, fulfill := src.Tee("split", func(o order) (order, order) {
	return cloneOrder(o), cloneOrder(o)
})
```

#### Cycles

`Send` merges a flow into the consumer another flow already feeds, so closing a cycle means passing the flow that **precedes** the node to re-enter, not the flow that node produces. The node being re-entered is therefore declared before the `Send` that closes the loop; a target with no consumer yet is a declaration error.

```golang
src, send := m.Source[int]("in")

incremented := src.Map("increment", func(f machine.Frame[int]) int { return f.Value() + 1 })
loop, done := incremented.If("under-five", func(f machine.Frame[int]) bool { return f.Value() < 5 })

loop.Send(src)
out := done.Output("out")
```

Feeding `0` into that graph yields `5`.

#### Recursion

Recursion is plain Go inside a `Map` body: declare a closure and call it. There is no recursion builder, because none is needed — the frame stays the runtime's, so declared handles keep working from inside the recursion exactly as they do anywhere else in the body.

Memoization is yours to place. A map closed over by the body memoizes **within one datum**; a heap `Cell` memoizes **across data**.

```golang
out := src.Map("fib", func(f machine.Frame[int]) int {
	cache := map[int]int{}
	var fib func(int) int
	fib = func(n int) int {
		if seen, ok := cache[n]; ok {
			return seen
		}
		if n < 2 {
			cache[n] = n
			return n
		}
		cache[n] = fib(n-1) + fib(n-2)
		return cache[n]
	}
	return fib(f.Value())
}).Output("out")
```

------

### **State**

State is reached **only** through the frame, and only where a node declared the capability for it. There are two namespaces, and they share one declaration namespace and one capability model.

| Handle | Scope | Declared with |
| --- | --- | --- |
| `Key[V]` | **Stack** — per-traversal, travels inside the frame, reclaimed when the traversal ends. | `NewKey(name, cloner)` |
| `Cell[V]` | **Heap** — machine-scoped, outlives every traversal. | `NewCell[V](name)` |

A stack key's cloner is **mandatory**: `Tee` deep-copies the frame into both branches, and only the caller knows how to copy `V`. Both constructors panic on an empty name or a name already declared as either kind, and `NewKey` panics on a nil cloner.

Capabilities are declared per node with `WithReads` and `WithWrites`, which take `KeyRef` so one declaration covers stack keys and heap cells together. **A write capability does not imply a read capability**, so a node that reads, modifies and writes one handle declares it in both.

```golang
var (
	attempts  = machine.NewKey("attempts", func(n int) int { return n })
	processed = machine.NewCell[int]("processed")
)

out := orders.Map("count", func(f machine.Frame[int]) int {
	// Stack: per-traversal.
	f.Set(attempts, f.Get(attempts)+1)

	// Heap: machine-scoped, updated atomically under the store's lock.
	f.Update(processed, func(n int) int { return n + 1 })

	return f.Value()
},
	machine.WithReads[int](attempts, processed),
	machine.WithWrites[int](attempts, processed),
).Output("out")
```

The type parameter on `WithReads` / `WithWrites` cannot be inferred from a `KeyRef` list, so the call site writes it.

#### Frame accessors

| Accessor | Capability | Returns |
| --- | --- | --- |
| `Value()` | none | The payload. |
| `ID()`, `Parent()`, `Source()`, `Node()` | none | Frame identity and lineage. `Parent` is empty for a frame born at a `Source`. |
| `Context()` | none | This node execution's span context. An outbound call made with it is CANCELED when the machine stops, and its span is parented to the node's span. It is execution-scoped and never serialized: a datum resumed from a remote transport runs under the resuming worker's context. |
| `Has(ref)` | read | Whether a declared handle currently holds a value — which is what distinguishes a handle never written from one holding the zero value. |
| `Get(k)` | read | The stack value, or the zero value of `V` if never written. |
| `Set(k, v)` | write | Writes a stack value. |
| `Load(c)` | read | The heap value and whether it was present. |
| `Save(c, v)` | write | Writes a heap value. |
| `Update(c, fn)` | read **and** write | Read-modify-write on a heap cell under the store's lock; returns the new value. |

An access a node did not declare raises a `*CapabilityError`. It is a panic rather than a returned error because a node function returns a bare payload and has no error slot; the supervisor's panic boundary routes it to the registered handler as a `NodeError`, where `errors.As` recovers it intact:

```golang
machine.WithErrorHandler(func(e machine.NodeError[int]) {
	var capErr *machine.CapabilityError
	if errors.As(e.Err, &capErr) {
		log.Printf("node %s has no %s capability for %s", capErr.Node, capErr.Access, capErr.Key)
	}
})
```

#### The host view

`Machine.Host()` is the **host-only** heap accessor. It exists so a program can seed heap cells before `Start` and inspect them after, from outside flow execution:

```golang
m.Host().Save(processed, 7)
value, ok := m.Host().Load(processed)
```

A node function must never call it — it bypasses the capability gate entirely, and a node reaches the heap through `Load`, `Save` and `Update`. Enforcement is structural and static; there is deliberately no runtime caller check, because a stack walk is unsound across goroutine boundaries.

The heap store itself is the `Store` interface, defaulting to `NewMemStore()`. Replace it with `OptionStore` to back the heap with something else; the replacement is still reached only through the gated frame accessors and through `Host`.

------

### **Errors**

With no handler registered the behavior is a **no-op drop**: no retry, no redelivery, no restart. Nothing is retained for redelivery — retry and dead-lettering are composed by you, inside a handler.

Register a global fallback with `OptionErrorHandler` and a typed per-node handler with `WithErrorHandler`. The per-node handler wins.

```golang
m := machine.New("orders",
	machine.OptionErrorHandler(func(e machine.NodeError[any]) {
		log.Printf("node %s failed: %v (panic=%t)", e.Node, e.Err, e.Panic)
	}),
)

src.Map("divide", func(f machine.Frame[int]) int {
	return 100 / f.Value() // a panic here is recovered and routed
},
	machine.WithErrorHandler(func(e machine.NodeError[int]) {
		log.Printf("divide rejected payload %d: %v", e.Payload, e.Err)
	}),
).Drop("done")
```

The global handler is `ErrorHandler[any]` because one machine's nodes carry many payload types; a per-node handler keeps `NodeError[T]` fully typed.

```golang
type NodeError[T any] struct {
	Node    string
	Err     error
	Payload T
	Panic   bool
}
```

`Panic` distinguishes a recovered panic from a returned error. Every node failure and every edge failure funnels through the same dispatch, so a transport error and a panicking node land in the same handler — but note **which** node they are attributed to: a send failure belongs to the node that produced the datum, while an edge's inbound refusal belongs to the node the edge delivers into.

`Start` refuses a graph that cannot run, reporting every problem at once: a duplicate node name, a flow that is never consumed, a flow consumed by two nodes, an edge factory that failed, an edge that refused to come up, a `Send` onto a target with no consumer, and instruments that could not be created.

------

### **Telemetry**

Instrumentation is direct OpenTelemetry — no logging wrapper. The machine resolves its tracer and meter **once**, at construction, from the providers you give it, defaulting to `otel.GetTracerProvider()` and `otel.GetMeterProvider()`. A provider registered globally after construction cannot reach an existing machine.

```golang
m := machine.New("observed",
	machine.WithTracerProvider(tracerProvider),
	machine.WithMeterProvider(meterProvider),
)
```

Configuring the SDK — exporters, resources, sampling — is yours. Passing a nil provider to either option is a declaration-time programmer error and panics rather than silently substituting the global.

Every span and metric is attributed to the instrumentation scope `machine.ScopeName` (`github.com/whitaker-io/machine/v4`) at version `machine.Version()`. Each datum a node processes opens a span named for that node, and three instruments are recorded:

| Instrument | Kind | Unit | Meaning |
| --- | --- | --- | --- |
| `machine.runs` | counter | `{datum}` | Data a node has begun processing. |
| `machine.errors` | counter | `{datum}` | Failures a node has reported to its error handler. |
| `machine.duration` | histogram | `s` | Time a node spent processing one datum. |

The instrument names carry forward from v3 so an existing dashboard keeps resolving; the duration unit deliberately does not, because v3 recorded milliseconds and OpenTelemetry states durations in seconds. The duration histogram runs for every datum, failed or not, so it counts **attempts**.

The only two attributes this package sets are `machine.name` and `machine.node`. Both carry node identity; nothing is derived from a datum. That is deliberate — an SDK aggregates a bounded number of attribute sets per instrument and folds everything past that into a single overflow set, so one unbounded payload-derived attribute would collapse every other series on the instrument along with it.

An `Output` node does not process, so it produces no span.

------

### **Transports**

A node owns its **inbound** edge, selected with `WithEdge`. The default is `Channel`, an in-memory channel edge, unbuffered:

```golang
src, send := m.Source[int]("in", machine.WithEdge(machine.Channel[int](64)))
```

Buffer size is a property of the edge rather than of the machine, so each edge is sized where it is constructed. The `http` and `pubsub` transports carry their own `WithBuffer` for the same reason.

A transport is any implementation of `Edge[T]`, constructed by an `EdgeFactory[T]`:

```golang
type Edge[T any] interface {
	Start(ctx context.Context) error
	Send(ctx context.Context, packet Packet[T]) error
	Receive() <-chan Packet[T]
	Close() error
}

type EdgeFactory[T any] func(node string, report Report) (Edge[T], error)

type Codec[T any] interface {
	Marshal(packet Packet[T]) ([]byte, error)
	Unmarshal(data []byte) (Packet[T], error)
}
```

`EdgeFactory` is a function type rather than an interface because Go forbids type parameters on interface methods. It is handed a `Report` — the path by which an edge hands the supervisor a failure that has **no datum to attribute**, such as a refused inbound message or a broken connection. `Close` must be idempotent: the runtime closes every constructed edge at shutdown and a caller may close one directly as well.

Edges carry `Packet` values rather than bare payloads, because the execution state must travel with the datum. A packet offers `Value()`, `ID()`, `Parent()`, `Source()`, `Node()` and `Data()`, and nothing else — it has no capability-gated accessor, so a transport cannot reach a node's state. `RebuildPacket` is the only exported way to make one, and it takes a wire projection rather than a frame. A remote transport marshals a packet with a `Codec`, and the shipped default is `GobCodec[T]` — gob rather than JSON because a stack value travels as an interface value, and JSON restores every number as `float64` and every struct as a map, which the receiving node's typed `Get` then fails on. A value type outside gob's built-ins must be `gob.Register`'d before it crosses a remote edge; gob's own error at marshal time is the enforcement.

#### edge/http

`github.com/whitaker-io/machine/edge/http`. One `Edge` value is **both halves** of a one-way hop: `Send` POSTs the marshaled envelope to a peer, and `ServeHTTP` accepts a peer's POST and delivers the rebuilt packet into the node this edge feeds. It is a one-way hop rather than a remote call, because a response body cannot carry the packet of a datum that is still traveling.

```golang
edge := http.New[Order]("https://peer.internal/orders")

// Receiving side: mount the same value as an http.Handler.
mux.Handle("/orders", edge)

// The node this edge delivers into.
src.Map("remote", handle, machine.WithEdge(edge.Factory()))
```

Options: `WithCodec` (defaults to `machine.GobCodec[T]{}`), `WithClient` (defaults to `http.DefaultClient`), `WithBuffer` (defaults to an unbuffered handoff) and `WithMaxBody` (defaults to 4 MiB).

A refused packet is loud on both sides: `ServeHTTP` reports the refusal locally **and** answers 400, and the peer's `Send` turns that 400 into an error its own supervisor routes. Trace context crosses in the request headers, injected by `Send` and extracted by `ServeHTTP`.

#### edge/pubsub

`github.com/whitaker-io/machine/edge/pubsub`. The Google Cloud Pub/Sub transport. `Send` publishes to a topic and a subscription delivers a peer's message into the node this edge feeds. It takes the **caller's** client, because credentials and endpoint configuration are yours.

```golang
edge := pubsub.New[Order](client, "orders-topic", "orders-subscription")

src.Map("remote", handle, machine.WithEdge(edge.Factory()))
```

Options: `WithCodec` and `WithBuffer`, which defaults to an unbuffered handoff so a slow node exerts backpressure on the callback rather than accumulating packets.

A refused message is reported and then **settled**. The report is the point; the acknowledgement is what stops the broker redelivering a message nothing in this process can ever decode. It is deliberately not nacked: with no redelivery limit a poison message would return forever. Nothing here retries, backs off or redelivers — those are the broker's and your concerns. Trace context rides the message attributes.

------

### **Options**

Machine options, passed to `machine.New`:

| Option | Effect |
| --- | --- |
| `OptionFIFO` | Forces serial processing: the machine waits for one datum to be processed before starting the next. |
| `OptionMaxConcurrency(n)` | Bounds the data a node processes at once when FIFO is off. Zero, the default, is unbounded. |
| `OptionErrorHandler(h)` | Registers the global fallback `ErrorHandler[any]`. |
| `OptionStore(s)` | Replaces the machine's heap `Store`. Defaults to `NewMemStore()`. |
| `WithTracerProvider(p)` | Sets the provider the machine resolves its tracer from. Defaults to `otel.GetTracerProvider()`. |
| `WithMeterProvider(p)` | Sets the provider the machine resolves its meter from. Defaults to `otel.GetMeterProvider()`. |

Node options, passed to any builder method:

| Option | Effect |
| --- | --- |
| `WithEdge(factory)` | Selects the transport that delivers **into** the node. Defaults to an unbuffered `Channel`. |
| `WithErrorHandler(h)` | Registers the node's typed handler, which wins over the machine's global one. |
| `WithReads(refs...)` | Declares the handles the node may read. |
| `WithWrites(refs...)` | Declares the handles the node may write. |

------

***
## 🤝 Contributing

Contributions, issues and feature requests are welcome.<br />
Feel free to check [issues page](https://github.com/whitaker-io/machine/issues) if you want to contribute.<br />
[Check the contributing guide](./CONTRIBUTING.md).<br />

## Author

👤 **Jonathan Whitaker**

- Twitter: [@io_whitaker](https://twitter.com/io_whitaker)
- Github: [@jonathan-whitaker](https://github.com/jonathan-whitaker)

## Show your support

Please ⭐️ this repository if this project helped you!

***
## [License](#license)

Machine is provided under the [MIT License](https://github.com/whitaker-io/machine/blob/master/LICENSE).

```text
The MIT License (MIT)

Copyright (c) 2020 Jonathan Whitaker
```
