// Package provider defines common errors for WebRTC providers.
package provider

import "errors"

var (
	// ErrProviderNotFound is returned when the requested provider is not registered.
	ErrProviderNotFound = errors.New("provider not found")
	// ErrDataChannelTimeout is returned when the DataChannel fails to open in time.
	ErrDataChannelTimeout = errors.New("datachannel timeout")
	// ErrDataChannelNotReady is returned when attempting to send data before the DataChannel is open.
	ErrDataChannelNotReady = errors.New("datachannel not ready")
	// ErrSendQueueClosed is returned when attempting to send data after the send queue has been closed.
	ErrSendQueueClosed = errors.New("send queue closed")
	// ErrSendQueueTimeout is returned when the send queue is full and the timeout is reached.
	ErrSendQueueTimeout = errors.New("send queue timeout")
	// ErrTransportNotImplemented is returned when a provider/transport name exists,
	// but its runtime media path has not been implemented yet.
	ErrTransportNotImplemented = errors.New("transport not implemented")
)
