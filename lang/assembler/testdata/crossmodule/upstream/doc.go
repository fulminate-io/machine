// Package upstream is the second module a cross-module `use` reaches.
//
// IT IS A REAL GO PACKAGE with a real go.mod, because the resolution under test
// is the loader's: Packages.ResolveFlow maps an import path to a module and
// reads the .flow files that module declares. A directory with no package clause
// would not be loaded at all, and the reference would refuse for that reason
// rather than for anything this fixture is about.
package upstream
