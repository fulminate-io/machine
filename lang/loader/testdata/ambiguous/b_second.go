// Package ambiguous - this second file binds codec to encoding/json where
// a_first.go binds it to encoding/gob. See a_first.go for what the pair is for.
package ambiguous

import (
	buf "bytes"
	codec "encoding/json"
)

// UsesSecond pins this file's codec alias to encoding/json.
var UsesSecond *codec.Encoder

// SecondBuffer pins this file's buf alias to bytes, the SAME package a_first.go
// binds it to.
var SecondBuffer buf.Buffer
