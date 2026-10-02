package mobile

import (
	"log"
	"os"
)

// LogWriter receives olcRTC's log lines. Implement it in Kotlin or Swift
// and hand it to SetLogWriter; the engine keeps whatever it is handed.
type LogWriter interface {
	WriteLog(msg string)
}

// SetLogWriter routes the process log to w. Nil restores stderr. The log is
// process-wide, so this is a package function rather than a Runtime method.
func SetLogWriter(w LogWriter) {
	if w == nil {
		log.SetOutput(os.Stderr)
		return
	}
	log.SetOutput(&logBridge{w: w})
}

// logBridge adapts LogWriter to io.Writer.
type logBridge struct {
	w LogWriter
}

func (b *logBridge) Write(p []byte) (int, error) {
	b.w.WriteLog(string(p))
	return len(p), nil
}
