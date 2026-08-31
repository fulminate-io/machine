// Package lsp - Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.
package lsp

import (
	"io"
	"testing"
	"time"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
)

// duplex joins one reader and one writer into a stream, which is exactly the
// shape cmd/flowlsp builds over the process's standard input and output.
type duplex struct {
	r io.Reader
	w io.WriteCloser
}

func (d duplex) Read(p []byte) (int, error)  { return d.r.Read(p) }
func (d duplex) Write(p []byte) (int, error) { return d.w.Write(p) }
func (d duplex) Close() error                { return d.w.Close() }

// TestTheServerServesOverAPipe drives Serve — the function main.go calls — to a
// completed initialize handshake over a pair of in-process pipes.
//
// IN-PROCESS RATHER THAN A SUBPROCESS, deliberately. main.go's whole content is
// the stream wiring, so spawning a binary would add a build step and a timeout
// while proving nothing this does not. Two io.Pipes joined into a duplex stream
// are the same shape main.go builds from stdin and stdout.
func TestTheServerServesOverAPipe(t *testing.T) {
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()

	served := make(chan error, 1)
	go func() { served <- Serve(t.Context(), duplex{r: serverIn, w: serverOut}) }()

	_, conn, remote := protocol.NewClient(t.Context(), &capturingClient{},
		jsonrpc2.NewStream(duplex{r: clientIn, w: clientOut}))
	t.Cleanup(func() { _ = conn.Close() })

	got, err := remote.Initialize(t.Context(), &protocol.InitializeParams{})
	if err != nil {
		t.Fatalf("the initialize handshake over the pipe failed: %v", err)
	}
	if got.ServerInfo.Name != serverName {
		t.Fatalf("the server identified itself as %q over the wire, want %q", got.ServerInfo.Name, serverName)
	}
	if got.Capabilities.PositionEncoding != protocol.PositionEncodingKindUTF16 {
		t.Fatalf("the capabilities that crossed the wire declare encoding %q, want %q",
			got.Capabilities.PositionEncoding, protocol.PositionEncodingKindUTF16)
	}

	// Serve must RETURN when the connection ends rather than hanging, or a real
	// editor closing its side would leave the process alive forever.
	_ = conn.Close()
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after the connection closed")
	}
}
