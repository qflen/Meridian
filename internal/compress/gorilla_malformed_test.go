package compress

import (
	"encoding/binary"
	"math"
	"testing"
)

// TestDecoderRejectsMalformedValueBlock feeds a hand-crafted stream whose second
// value block declares leading=31 and sigBits=40 (leading+sigBits=71 > 64). The
// decoder must reject it with an error rather than underflowing the trailing-zeros
// computation and silently corrupting output (or panicking). The encoder is not
// touched — this exercises the decode-path bounds-check only.
func TestDecoderRejectsMalformedValueBlock(t *testing.T) {
	var buf []byte

	// 4-byte big-endian count header (2 points).
	count := make([]byte, 4)
	binary.BigEndian.PutUint32(count, 2)
	buf = append(buf, count...)

	// First pair stored raw: timestamp (8) + value bits (8), big-endian.
	ts := make([]byte, 8)
	binary.BigEndian.PutUint64(ts, 1000)
	buf = append(buf, ts...)
	val := make([]byte, 8)
	binary.BigEndian.PutUint64(val, math.Float64bits(1.5))
	buf = append(buf, val...)

	// Second point bit stream (MSB first):
	//   ts control   = 0           (delta-of-delta 0)
	//   val control  = 1           (value changed)
	//   window       = 1           (new leading/trailing window)
	//   leading (5)  = 11111 (31)
	//   sigBits (6)  = 101000 (40)
	// => 0 1 1 11111 101000 packs to 0x7F 0xA0.
	buf = append(buf, 0x7F, 0xA0)

	dec := NewDecoder(buf)

	if !dec.Next() {
		t.Fatalf("first pair should decode, err=%v", dec.Err())
	}
	gotTs, gotVal := dec.Values()
	if gotTs != 1000 || gotVal != 1.5 {
		t.Fatalf("first pair decoded wrong: ts=%d val=%v", gotTs, gotVal)
	}

	if dec.Next() {
		t.Fatal("malformed second point must not decode")
	}
	if dec.Err() == nil {
		t.Fatal("expected a decode error for leading+sigBits > 64")
	}
}
