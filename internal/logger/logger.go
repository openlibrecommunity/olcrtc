//nolint:revive
package logger

import (
	"log"
	"sync"
	"sync/atomic"
)

type verboseFlag struct {
	enabled atomic.Bool
}

func getVerbose() *verboseFlag {
	once := getOnce()
	once.Do(func() {})
	return getInstance()
}

func getInstance() *verboseFlag {
	return &instance
}

func getOnce() *sync.Once {
	return &once
}

//nolint:gochecknoglobals
var instance verboseFlag

//nolint:gochecknoglobals
var once sync.Once

//nolint:revive
func SetVerbose(enabled bool) {
	getVerbose().enabled.Store(enabled)
}

//nolint:revive
func IsVerbose() bool {
	return getVerbose().enabled.Load()
}

//nolint:revive,goprintffuncname
func Verbose(format string, v ...any) {
	if getVerbose().enabled.Load() {
		log.Printf("[VERBOSE] "+format, v...)
	}
}

//nolint:revive,goprintffuncname
func Debug(format string, v ...any) {
	if getVerbose().enabled.Load() {
		log.Printf("[DEBUG] "+format, v...)
	}
}
