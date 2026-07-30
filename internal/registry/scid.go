package registry

import (
	"fmt"
	"strconv"
	"strings"
)

// A short channel id names where a channel's funding was confirmed: the block,
// the transaction's index within it, and the output's index within that.
//
// The two Lightning implementations spell it differently — Core Lightning as
// "850000x1x0", LND as the packed 64-bit integer — and the stored form is the
// readable one. Converting here rather than in each adapter is what keeps the
// same channel from being recorded two ways depending on which node reported it,
// which is precisely what happened before this existed.

// ShortChannelID is the stored spelling: BLOCKxTXxOUTPUT.
func ShortChannelID(block, txIndex, outputIndex uint32) string {
	return fmt.Sprintf("%dx%dx%d", block, txIndex, outputIndex)
}

// ParseShortChannelID reads the stored spelling back.
func ParseShortChannelID(scid string) (block, txIndex, outputIndex uint32, ok bool) {
	parts := strings.Split(strings.TrimSpace(scid), "x")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	values := make([]uint32, 3)
	for i, p := range parts {
		v, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return 0, 0, 0, false
		}
		values[i] = uint32(v)
	}
	return values[0], values[1], values[2], true
}

// ShortChannelIDFromPacked converts LND's packed form.
//
// The layout is fixed by the protocol: 24 bits of block height, 24 of
// transaction index, 16 of output index.
func ShortChannelIDFromPacked(packed uint64) (string, bool) {
	if packed == 0 {
		// A channel that has not confirmed has no short id, and inventing
		// "0x0x0" would read as one that confirmed in the genesis block.
		return "", false
	}
	block := uint32(packed >> 40 & 0xFFFFFF)
	txIndex := uint32(packed >> 16 & 0xFFFFFF)
	outputIndex := uint32(packed & 0xFFFF)
	return ShortChannelID(block, txIndex, outputIndex), true
}

// BlockFromShortChannelID reads the funding height out of a stored short id.
//
// Worth taking because it is free, and the alternative — asking the chain for a
// transaction older than a pruned node keeps — often fails on the hardware this
// ships to. Zero when there is no usable id, which is the honest answer for a
// channel that has not confirmed rather than a guess.
func BlockFromShortChannelID(scid string) int32 {
	block, _, _, ok := ParseShortChannelID(scid)
	if !ok || block >= 1<<31 {
		return 0
	}
	return int32(block)
}
