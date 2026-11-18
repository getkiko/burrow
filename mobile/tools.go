//go:build tools

// This file only pins a build-tool dependency in go.mod; it never takes part
// in a real build (the "tools" build tag is never satisfied).
//
// The gomobile fork github.com/sagernet/gomobile used for the bindings is not
// imported by any Go code; gobind only needs it while generating binding
// code. `go mod tidy` therefore treats it as unused and drops it, after which
// `make aar` fails with:
//
//	unable to import bind: no Go package in github.com/sagernet/gomobile/bind
//
// The blank import below declares the dependency explicitly so tidy keeps it.
package mobile

import _ "github.com/sagernet/gomobile/bind"
