package engine

import (
	"fmt"
	"net"
	"runtime"

	"github.com/pion/ice/v4"
	"github.com/pion/logging"
	"github.com/pion/webrtc/v4"

	"github.com/openlibrecommunity/olcrtc/internal/protect"
)

// PionSettingsOptions describes per-engine SettingEngine differences.
type PionSettingsOptions struct {
	Resolver         protect.Lookup
	LoggerFactory    logging.LoggerFactory
	IPv4Only         bool
	ProxyDialer      bool
	DisableMulticast bool
}

// PionSettings applies shared network settings to a pion SettingEngine.
type PionSettings func(*webrtc.SettingEngine)

// NewPionSettings prepares protected networking and per-engine pion settings.
func NewPionSettings(opts PionSettingsOptions) (PionSettings, error) {
	useProtectedNet := protect.HasProtector() || opts.Resolver != nil || runtime.GOOS == "android"
	var protectedNet *protect.ProtectedNet
	if useProtectedNet {
		var err error
		protectedNet, err = protect.NewProtectedNet(opts.Resolver)
		if err != nil {
			return nil, fmt.Errorf("protected net: %w", err)
		}
	}
	if opts.LoggerFactory == nil && !opts.IPv4Only && protectedNet == nil {
		return nil, nil //nolint:nilnil // nil hook preserves SDK-owned pion settings
	}

	return func(settings *webrtc.SettingEngine) {
		if opts.LoggerFactory != nil {
			settings.LoggerFactory = opts.LoggerFactory
		}
		if opts.IPv4Only {
			settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
			settings.SetIPFilter(func(ip net.IP) bool { return ip.To4() != nil })
		}
		if protectedNet == nil {
			return
		}
		settings.SetNet(protectedNet)
		if opts.ProxyDialer {
			settings.SetICEProxyDialer(protect.NewProxyDialer(opts.Resolver))
		}
		if opts.DisableMulticast {
			settings.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
		}
	}, nil
}
