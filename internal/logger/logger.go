package logger //nolint:revive

import (
	"log"
	"sync/atomic"
)

var verboseEnabled atomic.Bool //nolint:gochecknoglobals

func SetVerbose(enabled bool) { //nolint:revive
	verboseEnabled.Store(enabled)
}

func IsVerbose() bool { //nolint:revive
	return verboseEnabled.Load()
}

func Verbosef(format string, v ...interface{}) { //nolint:revive
	if verboseEnabled.Load() {
		log.Printf("[VERBOSE] "+format, v...)
	}
}

func Debugf(format string, v ...interface{}) { //nolint:revive
	if verboseEnabled.Load() {
		log.Printf("[DEBUG] "+format, v...)
	}
}
