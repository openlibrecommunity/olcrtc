//nolint:revive
package errors

import "errors"

//nolint:revive
var (
	ErrInvalidKeySize      = errors.New("invalid key size")
	ErrCiphertextTooShort  = errors.New("ciphertext too short")
	ErrDatachannelTimeout  = errors.New("datachannel timeout")
	ErrDatachannelNotReady = errors.New("datachannel not ready")
	ErrSendQueueClosed     = errors.New("send queue closed")
	ErrSendQueueTimeout    = errors.New("send queue timeout")
	ErrOnlyTelemost        = errors.New("only telemost provider supported")
	ErrRoomIDRequired      = errors.New("room ID required")
	ErrModeRequired        = errors.New("specify -mode srv or -mode cnc")
	ErrNoPeersAvailable    = errors.New("no peers available")
	ErrTooManyPeers        = errors.New("too many peers")
)

//nolint:revive
type KeySizeError struct {
	Got int
}

func (e KeySizeError) Error() string {
	return "key must be 32 bytes"
}

//nolint:revive
type KeyStringLengthError struct {
	Got int
}

func (e KeyStringLengthError) Error() string {
	return "key string length must be 32"
}

//nolint:revive
type APIError struct {
	StatusCode int
	Body       string
}

func (e APIError) Error() string {
	return "API error"
}
