// Package builtin registers the built-in carrier implementations.
package builtin

import (
	"github.com/openlibrecommunity/olcrtc/internal/carrier"
	"github.com/openlibrecommunity/olcrtc/internal/provider/jazz"
	"github.com/openlibrecommunity/olcrtc/internal/provider/telemost"
	"github.com/openlibrecommunity/olcrtc/internal/provider/wbstream"
)

// Register wires the built-in legacy carriers into the carrier registry.
func Register() {
	carrier.RegisterLegacy("jazz", jazz.New)
	carrier.RegisterLegacy("telemost", telemost.New)
	carrier.RegisterLegacy("wb_stream", wbstream.New)
}
