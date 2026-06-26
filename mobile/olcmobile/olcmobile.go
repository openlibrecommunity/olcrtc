// Package olcmobile — gomobile-обёртка olcrtc cnc для iOS/macOS (xcframework).
// Экспортирует StartCnc/Stop (простые типы — gomobile-совместимо). iOS NEPacketTunnelProvider
// зовёт StartCnc из фонового потока; tun2socks направляет пакеты tun в локальный SOCKS,
// который поднимает cnc. Stop отменяет (на iOS процесс расширения и так убивается системой).
package olcmobile

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	configpkg "github.com/openlibrecommunity/olcrtc/internal/config"
	"github.com/openlibrecommunity/olcrtc/internal/app/session"
)

var (
	mu     sync.Mutex
	cancel context.CancelFunc
)

// StartCnc запускает cnc-туннель по YAML-конфигу. БЛОКИРУЕТ до Stop()/ошибки —
// вызывать из фонового потока. dataDir — писабельная папка (app container);
// конфиг пишется туда же. SOCKS5 поднимается по socks.host/port из YAML.
func StartCnc(configYAML, dataDir string) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	cfgPath := filepath.Join(dataDir, "cnc.yaml")
	if err := os.WriteFile(cfgPath, []byte(configYAML), 0o600); err != nil {
		return err
	}
	session.RegisterDefaults()
	f, err := configpkg.Load(cfgPath)
	if err != nil {
		return err
	}
	scfg := configpkg.Apply(session.Config{}, f)
	if scfg, err = session.ApplyAuthDefaults(scfg); err != nil {
		return err
	}
	scfg = session.ApplyTransportDefaults(scfg)
	scfg = session.ApplyLivenessDefaults(scfg)
	if err := session.Validate(scfg); err != nil {
		return err
	}
	ctx, c := context.WithCancel(context.Background())
	mu.Lock()
	if cancel != nil {
		cancel()
	}
	cancel = c
	mu.Unlock()
	return session.Run(ctx, scfg)
}

// Stop отменяет активный туннель.
func Stop() {
	mu.Lock()
	defer mu.Unlock()
	if cancel != nil {
		cancel()
		cancel = nil
	}
}
