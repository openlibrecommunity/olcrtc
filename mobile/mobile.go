// Package mobile provides a gomobile-compatible API for olcRTC.
// Build with: gomobile bind -target=android ./mobile
package mobile

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/client"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/protect"
)

// SocketProtector protects sockets from VPN routing on Android.
// Implement this interface in Kotlin/Java and pass to SetProtector.
type SocketProtector interface {
	Protect(fd int) bool
}

// LogWriter receives log messages from olcRTC.
type LogWriter interface {
	WriteLog(msg string)
}

var (
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	ready  chan struct{}
	runErr error
)

// SetProtector sets the Android VPN socket protector.
// Must be called before Start.
func SetProtector(p SocketProtector) {
	if p == nil {
		protect.Protector = nil
		return
	}
	protect.Protector = func(fd int) bool {
		return p.Protect(fd)
	}
}

// SetLogWriter sets a custom log writer for olcRTC output.
func SetLogWriter(w LogWriter) {
	if w != nil {
		log.SetOutput(&logBridge{w: w})
	}
}

// SetDebug enables or disables verbose logging.
func SetDebug(enabled bool) {
	logger.SetVerbose(enabled)
	if enabled {
		log.SetFlags(log.Ltime | log.Lshortfile)
	} else {
		log.SetFlags(log.Ltime)
	}
}

// Start launches the olcRTC client in background.
// roomID: Telemost room ID (e.g. "xxx-xxx-xxx")
// keyHex: 64-char hex encryption key
// socksPort: local SOCKS5 proxy port (e.g. 10808)
// duo: use dual channels for higher throughput
// socksUser/socksPass: SOCKS5 credentials (empty = no auth)
func Start(roomID, keyHex string, socksPort int, duo bool, socksUser, socksPass string) error {
	mu.Lock()
	defer mu.Unlock()

	if cancel != nil {
		return fmt.Errorf("olcRTC already running")
	}

	if roomID == "" {
		return fmt.Errorf("roomID is required")
	}
	if keyHex == "" {
		return fmt.Errorf("keyHex is required")
	}

	roomURL := "https://telemost.yandex.ru/j/" + roomID

	ctx, c := context.WithCancel(context.Background())
	cancel = c
	done = make(chan struct{})
	ready = make(chan struct{})
	localReady := ready
	runErr = nil

	var readyOnce sync.Once

	go func() {
		err := client.RunWithReady(ctx, roomURL, keyHex, socksPort, duo, socksUser, socksPass, func() {
			readyOnce.Do(func() {
				close(localReady)
			})
		})
		mu.Lock()
		cancel = nil
		runErr = err
		mu.Unlock()
		close(done)
	}()

	return nil
}

// WaitReady blocks until the Telemost peers are connected and the local SOCKS5 listener is ready.
func WaitReady(timeoutMillis int) error {
	mu.Lock()
	r := ready
	d := done
	err := runErr
	running := cancel != nil
	mu.Unlock()

	if r == nil {
		if err != nil {
			return err
		}
		return fmt.Errorf("olcRTC is not running")
	}

	select {
	case <-r:
		return nil
	default:
	}

	if !running {
		if err != nil {
			return err
		}
		return fmt.Errorf("olcRTC stopped before becoming ready")
	}

	timer := time.NewTimer(time.Duration(timeoutMillis) * time.Millisecond)
	defer timer.Stop()

	select {
	case <-r:
		return nil
	case <-d:
		mu.Lock()
		err := runErr
		mu.Unlock()
		if err != nil {
			return err
		}
		return fmt.Errorf("olcRTC stopped before becoming ready")
	case <-timer.C:
		return fmt.Errorf("olcRTC start timed out")
	}
}

// Stop gracefully stops the olcRTC client.
func Stop() {
	mu.Lock()
	c := cancel
	d := done
	mu.Unlock()

	if c == nil {
		return
	}

	c()

	if d != nil {
		<-d
	}
}

// IsRunning returns true if the olcRTC client is active.
func IsRunning() bool {
	mu.Lock()
	defer mu.Unlock()
	return cancel != nil
}

// logBridge adapts LogWriter to io.Writer for log package.
type logBridge struct {
	w LogWriter
}

func (b *logBridge) Write(p []byte) (n int, err error) {
	b.w.WriteLog(string(p))
	return len(p), nil
}
