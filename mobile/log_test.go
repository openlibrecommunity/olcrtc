package mobile

import (
	"log"
	"os"
	"strings"
	"sync"
	"testing"
)

type recordingWriter struct {
	mu    sync.Mutex
	lines []string
}

func (w *recordingWriter) WriteLog(msg string) {
	w.mu.Lock()
	w.lines = append(w.lines, msg)
	w.mu.Unlock()
}

func TestSetLogWriterReceivesTheProcessLog(t *testing.T) {
	w := &recordingWriter{}
	SetLogWriter(w)
	t.Cleanup(func() { SetLogWriter(nil); log.SetOutput(os.Stderr) })
	log.Print("hello from olcrtc")
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.lines) != 1 || !strings.Contains(w.lines[0], "hello from olcrtc") {
		t.Fatalf("writer got %q", w.lines)
	}
}
