package logger

import (
	"fmt"
	"log"
	"sync/atomic"
)

var verboseEnabled atomic.Bool

// SetVerbose enables or disables verbose/debug logging.
func SetVerbose(enabled bool) {
	verboseEnabled.Store(enabled)
}

// IsVerbose returns true if verbose logging is enabled.
func IsVerbose() bool {
	return verboseEnabled.Load()
}

// Info logs an informational message.
func Info(v ...any) {
	log.Print("[INFO] ", fmt.Sprint(v...))
}

// Infof logs a formatted informational message.
func Infof(format string, v ...any) {
	log.Printf("[INFO] "+format, v...)
}

// Warn logs a warning message.
func Warn(v ...any) {
	log.Print("[WARN] ", fmt.Sprint(v...))
}

// Warnf logs a formatted warning message.
func Warnf(format string, v ...any) {
	log.Printf("[WARN] "+format, v...)
}

// Error logs an error message.
func Error(v ...any) {
	log.Print("[ERROR] ", fmt.Sprint(v...))
}

// Errorf logs a formatted error message.
func Errorf(format string, v ...any) {
	log.Printf("[ERROR] "+format, v...)
}

// Verbosef logs a formatted message if verbose logging is enabled.
func Verbosef(format string, v ...any) {
	if verboseEnabled.Load() {
		log.Printf("[VERBOSE] "+format, v...)
	}
}

// Debugf logs a formatted message if verbose logging is enabled.
func Debugf(format string, v ...any) {
	if verboseEnabled.Load() {
		log.Printf("[DEBUG] "+format, v...)
	}
}
