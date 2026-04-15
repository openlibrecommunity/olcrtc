// Package transport implements the framing layer that sits between the
// Jazz mediaEngine and the pure-Go visual codec.
//
// # What this layer does
//
// The codec (internal/visual/codec) knows how to pack up to MaxPayloadBytes
// (297) bytes into a 640×480 I420 frame. The mediaEngine above it needs to
// carry upstream application messages of up to Jazz's maxDataChannelMessage
// Size (12288 bytes), which is ~41× larger. This package bridges that gap:
//
//  1. split a message into equally-sized Reed-Solomon shards;
//  2. wrap each shard with a 13-byte slot header carrying a per-slot CRC16,
//     the owning message id, and the shard index within the RS block;
//  3. emit slots as ready-to-pass byte buffers of length ≤ MaxPayloadBytes;
//  4. on the receiving side, collect slots by message id, drop corrupted
//     ones (CRC fail), and reconstruct the original message as soon as any
//     N shards out of N+M have arrived.
//
// The layer is completely agnostic of how slots actually cross the wire —
// that is the mediaEngine's job, and in tests we simply feed slots through
// codec.Encode/codec.Decode with injected noise. The plan calls this the
// "NullProvider loopback" at stage 1, but here it specifically validates
// the framing against the *visual* codec rather than a byte loopback.
//
// # Slot layout
//
//	offset  size  field
//	0       1     magic      (0xAA, sanity byte — rejects obvious noise)
//	1       1     version    (0x01, bumps let us change the format later)
//	2       4     msgID      (uint32 big-endian, wraps; see Fragmenter)
//	6       2     N          (uint16 big-endian, data shards per message)
//	8       1     M          (uint8,  parity shards per message)
//	9       2     index      (uint16 big-endian, 0 ≤ index < N+M)
//	11      2     crc16      (CRC-16-CCITT-FALSE over bytes [0..11) + shard)
//	13      ...   shard      (RS shard data, exact length varies per msg)
//
// Shard length inside a message is fixed by the RS encoder and equals
// (len(payload) + 4) / N rounded up, where the "+ 4" is the length prefix
// stored by internal/visual/fec. The Reassembler infers the shard length
// from the first slot it sees in a message.
//
// # Per-slot CRC rationale
//
// We could rely on the underlying codec's sync-corner check and the RS
// decoder's implicit integrity. In practice the codec will see *some*
// symbols flip after video re-encode, and we want to treat a single
// corrupted slot as "lost" rather than feed it to RS — which would taint
// the whole block. A cheap 16-bit CRC on the slot bytes lets us do that
// without adding a whole extra FEC level.
//
// # Header constants
//
// The header is deliberately small so that short messages (≤ ~250 bytes
// of payload) still fit in one or two slots and incur close to zero
// transport overhead.
package transport

import "encoding/binary"

const (
	// HeaderSize is the fixed byte length of a slot header.
	HeaderSize = 13

	// magicByte sits in slot[0]. If the receiver sees anything else
	// after a codec.Decode success it treats the slot as garbage and
	// drops it without touching the reassembly state machine.
	magicByte uint8 = 0xAA

	// formatVersion is slot[1]. Bump it whenever the header layout or
	// semantics change in a non-backwards-compatible way.
	formatVersion uint8 = 0x01
)

// Header is the parsed representation of the 13-byte slot header. Exposed
// for tests and higher layers that want to inspect routing / reassembly
// decisions; the actual on-wire packing lives in packSlot / parseSlot.
type Header struct {
	MsgID uint32
	N     uint16
	M     uint8
	Index uint16
}

// Total returns the total number of shards (data + parity) in the message
// this slot belongs to.
func (h Header) Total() uint16 {
	return h.N + uint16(h.M)
}

// packSlot builds a fully populated slot byte buffer from a header and the
// shard payload. The caller must have already constructed the shard (i.e.
// fec.Encoder.Encode output) and knows the correct index.
//
// The CRC covers everything in the slot *except* the CRC field itself.
// Writing CRC last means we can compute it in-place without a scratch
// buffer.
func packSlot(h Header, shard []byte) []byte {
	slot := make([]byte, HeaderSize+len(shard))
	slot[0] = magicByte
	slot[1] = formatVersion
	binary.BigEndian.PutUint32(slot[2:6], h.MsgID)
	binary.BigEndian.PutUint16(slot[6:8], h.N)
	slot[8] = h.M
	binary.BigEndian.PutUint16(slot[9:11], h.Index)

	// Shard payload must be copied in *before* we compute CRC so that
	// the CRC covers the exact bytes that will be sent.
	copy(slot[HeaderSize:], shard)

	// Slots 11..13 (the CRC field) are still zero at this point, which
	// is fine because we exclude them from the checksum. Writing them
	// in at the end closes the packet.
	crc := crc16CCITT(slot[:11], slot[HeaderSize:])
	binary.BigEndian.PutUint16(slot[11:13], crc)

	return slot
}

// parseSlot validates and unpacks a slot. Returns (header, shardPayload,
// true) on a healthy slot, or (Header{}, nil, false) if any of magic,
// version, length, or CRC is wrong.
//
// The returned shard slice aliases the input — callers that keep it past
// the next Accept() call should copy it.
func parseSlot(slot []byte) (Header, []byte, bool) {
	if len(slot) < HeaderSize {
		return Header{}, nil, false
	}
	if slot[0] != magicByte || slot[1] != formatVersion {
		return Header{}, nil, false
	}

	shard := slot[HeaderSize:]
	storedCRC := binary.BigEndian.Uint16(slot[11:13])
	computedCRC := crc16CCITT(slot[:11], shard)
	if storedCRC != computedCRC {
		return Header{}, nil, false
	}

	return Header{
		MsgID: binary.BigEndian.Uint32(slot[2:6]),
		N:     binary.BigEndian.Uint16(slot[6:8]),
		M:     slot[8],
		Index: binary.BigEndian.Uint16(slot[9:11]),
	}, shard, true
}

// crc16CCITT computes the CRC-16-CCITT-FALSE variant (poly 0x1021, init
// 0xFFFF, no reflection, no xor-out). It is the simplest 16-bit CRC that
// has good random-error detection properties and is easy to reimplement on
// any peer side we care about. We compute over two byte slices without
// concatenating them to avoid an allocation on the hot path.
func crc16CCITT(parts ...[]byte) uint16 {
	var crc uint16 = 0xFFFF
	for _, part := range parts {
		for _, b := range part {
			crc ^= uint16(b) << 8
			for range 8 {
				if crc&0x8000 != 0 {
					crc = (crc << 1) ^ 0x1021
				} else {
					crc <<= 1
				}
			}
		}
	}
	return crc
}
