// Package screening is the second module the cross-module `use` reaches.
//
// IT IS A REAL GO PACKAGE, and this file exists for that reason alone. The
// resolution under test is the loader's: Packages.ResolveFlow maps an import
// path to a MODULE and reads the .flow files that module declares, so a
// directory carrying a go.mod and a .flow but no package clause is never loaded
// and the reference refuses with `no loaded package has a module for import
// path ...` — a refusal about the fixture rather than about anything the smoke
// is observing. The first draft of this fixture omitted it and drew exactly
// that message.
package screening
