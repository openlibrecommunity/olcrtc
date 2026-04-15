package jazz

import "sync"

var visualBackgroundConfig struct { //nolint:gochecknoglobals
	mu   sync.RWMutex
	path string
}

// SetVisualBackgroundPath configures a static I420 carrier file for LAV mode.
func SetVisualBackgroundPath(path string) {
	visualBackgroundConfig.mu.Lock()
	visualBackgroundConfig.path = path
	visualBackgroundConfig.mu.Unlock()
}

func getVisualBackgroundPath() string {
	visualBackgroundConfig.mu.RLock()
	defer visualBackgroundConfig.mu.RUnlock()
	return visualBackgroundConfig.path
}
