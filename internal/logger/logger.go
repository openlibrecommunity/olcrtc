package logger

import (
	"log"
	"sync/atomic"
)

var verboseEnabled atomic.Bool

func SetVerbose(enabled bool) {
	verboseEnabled.Store(enabled)
}

func IsVerbose() bool {
	return verboseEnabled.Load()
}

func Verbose(format string, v ...interface{}) {
	if verboseEnabled.Load() {
		log.Printf("[VERBOSE] "+format, v...)
	}
}

func Debug(format string, v ...interface{}) {
	if verboseEnabled.Load() {
		log.Printf("[DEBUG] "+format, v...)
	}
}
