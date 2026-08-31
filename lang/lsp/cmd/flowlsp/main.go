// Command flowlsp - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// Command flowlsp serves the flow language's analysis over the Language Server
// Protocol, speaking the base protocol on standard input and output.
//
// IT CONTAINS NO LSP LOGIC. The transport framing belongs to jsonrpc2 and every
// handler belongs to the lsp package, so this file is the stream wiring and
// nothing else — which is what lets the whole server be exercised in-process
// over a pipe instead of through a spawned subprocess.
//
// STDIO IS THE ONLY TRANSPORT. jsonrpc2 can also serve over a net.Listener, but
// every editor integration this version targets spawns the server and speaks
// over its standard streams, and an unused listener is an unexercised surface.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/whitaker-io/machine/lang/lsp"
)

// stdio joins the process's standard input and output into the one duplex
// stream the server needs, since neither half is duplex on its own.
type stdio struct{}

func (stdio) Read(p []byte) (int, error) { return os.Stdin.Read(p) }

func (stdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }

// Close closes both halves and reports whatever either one said. It does not
// swallow the errors: a stream that failed to close is worth knowing about even
// as the process ends.
func (stdio) Close() error { return errors.Join(os.Stdin.Close(), os.Stdout.Close()) }

func main() {
	if err := lsp.Serve(context.Background(), stdio{}); err != nil {
		// Standard error is the only channel left: standard output belongs to
		// the protocol, so a message written there would corrupt the stream a
		// client is still reading.
		_, _ = fmt.Fprintln(os.Stderr, "flowlsp:", err)
		os.Exit(1)
	}
}
