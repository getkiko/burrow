// Package mobile is the gomobile binding of the tunnel package for Android.
//
// Design notes:
//
//   - Config and status are passed as JSON strings. gomobile cannot bind
//     maps, nested structs, time.Duration, uint64 or cross-package pointers;
//     JSON lets fields evolve without changing the binding layer and the
//     Kotlin side in lockstep.
//
//   - Global single instance. Android never needs more than one tunnel at a
//     time, and a singleton means Stop and Snapshot need no handle argument.
//     A second Start returns an explicit error instead of silently replacing
//     the running tunnel.
//
//   - Every exported function recovers from panics. A Go panic does not
//     become a Java exception; it kills the whole app process with a fatal
//     crash that Kotlin's try/catch cannot catch, because the gomobile
//     runtime never recovers.
package mobile

import "fmt"

// recoverToError converts a panic into an error, for exported functions that
// return an error:
//
//	func Exported() (err error) {
//	    defer recoverToError(&err)
//	    ...
//	}
func recoverToError(err *error) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("internal error (panic): %v", r)
	}
}

// recoverToString replaces the result with a fallback value on panic, for
// exported functions that return only a string.
func recoverToString(out *string, fallback string) {
	if r := recover(); r != nil {
		*out = fallback
	}
}

// silentRecover swallows a panic. It is used by the callback bridge: a panic
// in a host-implemented callback must not take down the Go code that is
// punching or forwarding data.
func silentRecover() {
	_ = recover()
}
