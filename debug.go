package burrow

import (
	"log"
	"os"
	"sync/atomic"
)

// debugEnabled controls internal diagnostic logging. It is off by default:
// punching involves many retries and goroutines, and unconditional logging
// would drown the caller's output.
//
// Set BURROW_DEBUG=1 to enable it, which helps track down problems such as
// "connected but no traffic" that only reproduce under concurrency.
var debugEnabled atomic.Bool

// LogFunc receives log lines from burrow. Callers can route it into their own
// leveled logger.
type LogFunc func(format string, args ...any)

// DebugLogFunc is an alias of LogFunc kept for existing callers.
type DebugLogFunc = LogFunc

func init() {
	if os.Getenv("BURROW_DEBUG") == "1" {
		debugEnabled.Store(true)
	}
}

// SetDebug turns diagnostic logging on or off at run time. It only affects
// output when no Config.DebugLog is set.
func SetDebug(on bool) { debugEnabled.Store(on) }

func debugLogf(logger DebugLogFunc, format string, args ...any) {
	if logger != nil {
		logger(format, args...)
		return
	}
	debugf(format, args...)
}

func infoLogf(logger LogFunc, format string, args ...any) {
	if logger != nil {
		logger(format, args...)
	}
}

func debugf(format string, args ...any) {
	if debugEnabled.Load() {
		log.Printf("[burrow] "+format, args...)
	}
}

// shortSession truncates a session ID to 8 characters for logs.
func shortSession(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
