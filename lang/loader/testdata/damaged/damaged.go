// Package damaged is a fixture module whose FLOW source does not parse.
//
// Its Go source is deliberately fine: the refusal under test is about the .flow
// file, and a module that failed to LOAD would exercise a different path
// entirely. It lives in a module of its own so that one unparseable source
// cannot poison every other assertion in the loader's suite.
package damaged
