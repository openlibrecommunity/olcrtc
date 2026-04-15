package jazz

import "sync"

var visualDumpConfig struct { //nolint:gochecknoglobals
	mu  sync.RWMutex
	dir string
}

// SetVisualDumpDir configures the directory used for periodic PNG frame dumps.
func SetVisualDumpDir(dir string) {
	visualDumpConfig.mu.Lock()
	visualDumpConfig.dir = dir
	visualDumpConfig.mu.Unlock()
}

func getVisualDumpDir() string {
	visualDumpConfig.mu.RLock()
	defer visualDumpConfig.mu.RUnlock()
	return visualDumpConfig.dir
}
