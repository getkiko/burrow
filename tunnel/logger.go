package tunnel

import (
	"fmt"
	"log"
	"os"
	"strings"
)

// Log levels.
const (
	levelDebug = iota
	levelInfo
	levelWarn
	levelError
)

// LogSink receives one log line. level is "debug", "info", "warn" or "error".
//
// It lets a host (such as the Android bindings) feed logs into its own UI:
// by default Logger writes only to stderr, which is invisible on mobile.
//
// Implementations must be safe for concurrent use; logs may come from many
// goroutines.
type LogSink func(level, line string)

// Logger is a simple level-filtered logger that writes to stderr.
//
// When a LogSink is set, every line is also passed to the sink.
type Logger struct {
	level  int
	stdlog *log.Logger
	sink   LogSink
}

// newLogger returns a stderr-only Logger for the named level. Unknown levels
// are treated as info.
func newLogger(levelName string) *Logger {
	return newLoggerWithSink(levelName, nil)
}

// newLoggerWithSink returns a Logger that also feeds sink. With a nil sink it
// behaves exactly like newLogger.
func newLoggerWithSink(levelName string, sink LogSink) *Logger {
	level := levelInfo
	switch strings.ToLower(levelName) {
	case "debug":
		level = levelDebug
	case "info":
		level = levelInfo
	case "warn":
		level = levelWarn
	case "error":
		level = levelError
	}
	return &Logger{
		level:  level,
		stdlog: log.New(os.Stderr, "", log.LstdFlags),
		sink:   sink,
	}
}

func (l *Logger) logf(level int, prefix, format string, args ...any) {
	if l == nil || level < l.level {
		return
	}
	line := fmt.Sprintf(format, args...)
	l.stdlog.Printf("[%s] %s", prefix, line)
	l.emitToSink(prefix, line)
}

// emitToSink passes a line to the sink and contains any panic it raises.
//
// Logging is a side channel: the sink is host code (on Android it calls
// across JNI into Kotlin), and its failures must not take down punching or
// forwarding.
func (l *Logger) emitToSink(level, line string) {
	if l.sink == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			// stderr only: the sink itself is untrustworthy at this point.
			l.stdlog.Printf("[error] log sink panic: %v", r)
		}
	}()
	l.sink(level, line)
}

func (l *Logger) Debugf(format string, args ...any) { l.logf(levelDebug, "debug", format, args...) }
func (l *Logger) Infof(format string, args ...any)  { l.logf(levelInfo, "info", format, args...) }
func (l *Logger) Warnf(format string, args ...any)  { l.logf(levelWarn, "warn", format, args...) }
func (l *Logger) Errorf(format string, args ...any) { l.logf(levelError, "error", format, args...) }

// SDKDebugFunc returns a callback suitable for burrow.Config.DebugLog.
func (l *Logger) SDKDebugFunc() func(string, ...any) {
	return func(format string, args ...any) {
		l.Debugf(format, args...)
	}
}

// SDKInfoFunc returns a callback suitable for burrow.Config.InfoLog.
func (l *Logger) SDKInfoFunc() func(string, ...any) {
	return func(format string, args ...any) {
		l.Infof(format, args...)
	}
}
