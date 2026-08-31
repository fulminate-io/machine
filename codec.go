// Copyright © 2020 Jonathan Whitaker <github@whitaker.io>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package machine

import (
	"bytes"
	"encoding/gob"
	"fmt"
)

// envelope is what a remote transport puts on the wire: the frame's stack projection
// beside the payload, so lineage and declared keys cross the hop together.
type envelope[T any] struct {
	Frame   FrameData
	Payload T
}

// GobCodec is the default remote codec, shared by every transport this repo ships.
//
// It is gob rather than JSON because a stack value travels as an interface value
// inside FrameData.Values: JSON restores every number as float64 and every struct as
// a map, and the receiving node's typed Get then fails on the restored value. Gob
// transmits the concrete type instead.
//
// A value type outside gob's built-ins must be gob.Register'd before it crosses a
// remote edge. Nothing here checks that: gob's own loud error at Marshal is the
// enforcement.
type GobCodec[T any] struct{}

// Marshal projects the frame's stack state beside its payload and encodes both as one
// self-contained gob stream.
func (GobCodec[T]) Marshal(frame Frame[T]) ([]byte, error) {
	body := envelope[T]{Frame: frame.Data(), Payload: frame.Value()}
	var sink bytes.Buffer
	if err := gob.NewEncoder(&sink).Encode(body); err != nil {
		return nil, fmt.Errorf("machine: encoding frame %q failed: %w", body.Frame.ID, err)
	}
	return sink.Bytes(), nil
}

// Unmarshal decodes a gob stream and rebuilds the frame, so a projection naming an
// undeclared stack key is refused by RebuildFrame rather than here.
func (GobCodec[T]) Unmarshal(data []byte) (Frame[T], error) {
	var body envelope[T]
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&body); err != nil {
		return Frame[T]{}, fmt.Errorf("machine: decoding a frame failed: %w", err)
	}
	return RebuildFrame(body.Frame, body.Payload)
}
