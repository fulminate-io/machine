// Package ambiguous binds ONE alias to TWO different packages across its two
// files, which is legal Go and makes a qualified spelling genuinely undecidable
// at package level.
//
// This first file binds codec to encoding/gob. The second binds the same alias
// to encoding/json. Both packages export Encoder, so the spelling
// `codec.Encoder` resolves in both files to two different types and no answer is
// more correct than the other — the only truthful response is to refuse and name
// both.
//
// Both files also bind buf to the SAME package, so `buf.Buffer` resolves twice
// to one type. That is the control: it proves a refusal comes from genuine
// disagreement rather than from merely resolving in more than one file.
package ambiguous

import (
	buf "bytes"
	codec "encoding/gob"
)

// UsesFirst pins this file's codec alias to encoding/gob.
var UsesFirst *codec.Encoder

// FirstBuffer pins this file's buf alias to bytes.
var FirstBuffer buf.Buffer
