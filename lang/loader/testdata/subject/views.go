// Package subject's SECOND file, which exists to give the multi-file resolution
// walk something to walk.
//
// THE FILENAME SORTS AFTER subject.go DELIBERATELY. Resolution visits a
// package's files in filename order, so a spelling that only this file's import
// can resolve is unreachable to any implementation that stops at the first file
// — which is exactly what the leg using codec.Marshaler pins.
//
// The alias differs from subject.go's import on purpose too: within one package,
// two files can bind entirely different packages, and a resolver that treats a
// package as having one flat import set is wrong in a way only a second file
// reveals.
package subject

import codec "encoding/json"

// View carries a type reachable only through THIS file's import alias.
type View struct {
	Marshal codec.Marshaler
}
