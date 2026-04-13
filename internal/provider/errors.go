package provider

import "errors"

var (
	ErrProviderNotFound    = errors.New("provider not found")
	ErrDataChannelTimeout  = errors.New("datachannel timeout")
	ErrDataChannelNotReady = errors.New("datachannel not ready")
	ErrSendQueueClosed     = errors.New("send queue closed")
	ErrSendQueueTimeout    = errors.New("send queue timeout")
	ErrSessionClosed       = errors.New("session closed")
	ErrPeerClosed          = errors.New("peer closed")
)
