// Package compress implements Facebook's Gorilla time-series compression algorithm.
//
// The algorithm is described in the 2015 paper "Gorilla: A Fast, Scalable, In-Memory
// Time Series Database." It achieves >10x compression on regular-interval metrics by
// exploiting the fact that consecutive timestamps have similar deltas and consecutive
// values are often identical or differ only in a few bits.
package compress

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"sync"
)

// Encoder compresses time-series data using Gorilla's delta-of-delta timestamp
// encoding and XOR-based float compression.
type Encoder struct {
	buf []byte // compressed output buffer

	// Bit-level write position
	bw       uint // byte offset into buf for current byte being written
	bitCount uint // bits written into current byte (0–7), MSB first

	// Timestamp state
	tPrev      int64
	tDeltaPrev int64

	// Value state
	vPrev          uint64
	vLeadingZeros  uint8
	vTrailingZeros uint8

	count int
}

// NewEncoder returns a new Gorilla encoder.
func NewEncoder() *Encoder {
	return &Encoder{
		buf:            make([]byte, 0, 1024),
		vLeadingZeros:  255, // sentinel: no previous window
		vTrailingZeros: 0,
	}
}

// Write appends a timestamp-value pair to the compressed stream.
func (e *Encoder) Write(timestamp int64, value float64) {
	if e.count == 0 {
		e.writeFirst(timestamp, value)
	} else {
		e.writeTimestamp(timestamp)
		e.writeValue(value)
	}
	e.count++
}

// Bytes returns the compressed data including a 4-byte count header.
// The caller must not modify the returned slice.
func (e *Encoder) Bytes() []byte {
	if e.count == 0 {
		return nil
	}
	// Build output: 4-byte count + compressed payload
	end := e.bw
	if e.bitCount > 0 {
		end = e.bw + 1
	}
	if end > uint(len(e.buf)) {
		end = uint(len(e.buf))
	}

	out := make([]byte, 4+end)
	binary.BigEndian.PutUint32(out[:4], uint32(e.count))
	copy(out[4:], e.buf[:end])
	return out
}

// Count returns the number of data points written.
func (e *Encoder) Count() int {
	return e.count
}

func (e *Encoder) writeFirst(timestamp int64, value float64) {
	// Store the first timestamp raw (64 bits)
	e.buf = e.buf[:0]
	e.appendBytes(8)
	binary.BigEndian.PutUint64(e.buf[0:8], uint64(timestamp))

	// Store the first value raw (64 bits)
	vBits := math.Float64bits(value)
	e.appendBytes(8)
	binary.BigEndian.PutUint64(e.buf[8:16], vBits)

	e.tPrev = timestamp
	e.tDeltaPrev = 0
	e.vPrev = vBits
	e.vLeadingZeros = 255 // sentinel
	e.vTrailingZeros = 0

	// Position the bit writer after the 16 raw bytes
	e.bw = 16
	e.bitCount = 0
}

func (e *Encoder) writeTimestamp(timestamp int64) {
	delta := timestamp - e.tPrev
	dod := delta - e.tDeltaPrev

	switch {
	case dod == 0:
		e.writeBit(0)
	case dod >= -63 && dod <= 64:
		e.writeBits(0b10, 2)
		e.writeBitsSigned(dod, 63, 7)
	case dod >= -255 && dod <= 256:
		e.writeBits(0b110, 3)
		e.writeBitsSigned(dod, 255, 9)
	case dod >= -2047 && dod <= 2048:
		e.writeBits(0b1110, 4)
		e.writeBitsSigned(dod, 2047, 12)
	default:
		e.writeBits(0b1111, 4)
		e.writeBits(uint64(dod), 64)
	}

	e.tDeltaPrev = delta
	e.tPrev = timestamp
}

func (e *Encoder) writeBitsSigned(val int64, offset int64, nbits uint) {
	e.writeBits(uint64(val+offset), nbits)
}

func (e *Encoder) writeValue(value float64) {
	vBits := math.Float64bits(value)
	xor := vBits ^ e.vPrev

	if xor == 0 {
		e.writeBit(0)
	} else {
		e.writeBit(1)

		leading := uint8(bits.LeadingZeros64(xor))
		trailing := uint8(bits.TrailingZeros64(xor))

		// Cap leading zeros to 31 (5-bit field)
		if leading > 31 {
			leading = 31
		}

		if e.vLeadingZeros != 255 && leading >= e.vLeadingZeros && trailing >= e.vTrailingZeros {
			// Fits in previous window — use control bit '0'
			e.writeBit(0)
			sigBits := 64 - int(e.vLeadingZeros) - int(e.vTrailingZeros)
			e.writeBits(xor>>e.vTrailingZeros, uint(sigBits))
		} else {
			// New window — use control bit '1'
			e.writeBit(1)
			e.writeBits(uint64(leading), 5)

			sigBits := 64 - int(leading) - int(trailing)
			if sigBits == 64 {
				// Special: 0 means 64 in the 6-bit field
				e.writeBits(0, 6)
			} else {
				e.writeBits(uint64(sigBits), 6)
			}
			e.writeBits(xor>>trailing, uint(sigBits))

			e.vLeadingZeros = leading
			e.vTrailingZeros = trailing
		}
	}

	e.vPrev = vBits
}

func (e *Encoder) writeBit(v byte) {
	if e.bw >= uint(len(e.buf)) {
		e.appendBytes(1)
	}
	if v != 0 {
		e.buf[e.bw] |= 1 << (7 - e.bitCount)
	}
	e.bitCount++
	if e.bitCount == 8 {
		e.bw++
		e.bitCount = 0
	}
}

func (e *Encoder) writeBits(v uint64, nbits uint) {
	for i := int(nbits) - 1; i >= 0; i-- {
		bit := byte((v >> uint(i)) & 1)
		e.writeBit(bit)
	}
}

func (e *Encoder) appendBytes(n int) {
	for i := 0; i < n; i++ {
		e.buf = append(e.buf, 0)
	}
}

// Decoder decompresses Gorilla-encoded time-series data.
type Decoder struct {
	buf []byte

	// Bit-level read position
	br       uint // byte offset
	bitCount uint // bits consumed in current byte (0–7), MSB first

	// Timestamp state
	tPrev      int64
	tDeltaPrev int64

	// Value state
	vPrev          uint64
	vLeadingZeros  uint8
	vTrailingZeros uint8

	// Current decoded pair
	curTimestamp int64
	curValue    float64

	total int // total points in stream
	read  int // points decoded so far
	err   error
	done  bool
}

// NewDecoder returns a decoder for Gorilla-compressed data.
func NewDecoder(data []byte) *Decoder {
	if len(data) < 4 {
		return &Decoder{done: true}
	}
	total := int(binary.BigEndian.Uint32(data[:4]))
	if total == 0 {
		return &Decoder{done: true}
	}
	return &Decoder{
		buf:            data[4:], // skip count header
		total:          total,
		vLeadingZeros:  255,
		vTrailingZeros: 0,
	}
}

// Next advances to the next data point. Returns false when no more data is available.
func (d *Decoder) Next() bool {
	if d.done || d.err != nil || d.read >= d.total {
		return false
	}

	if d.read == 0 {
		return d.readFirst()
	}

	return d.readNext()
}

// Values returns the current timestamp and value. Only valid after Next() returns true.
func (d *Decoder) Values() (int64, float64) {
	return d.curTimestamp, d.curValue
}

// Err returns the first decoding error encountered, if any.
func (d *Decoder) Err() error {
	return d.err
}

func (d *Decoder) readFirst() bool {
	if len(d.buf) < 16 {
		d.err = fmt.Errorf("buffer too small for first pair: %d bytes", len(d.buf))
		d.done = true
		return false
	}

	d.curTimestamp = int64(binary.BigEndian.Uint64(d.buf[0:8]))
	d.curValue = math.Float64frombits(binary.BigEndian.Uint64(d.buf[8:16]))

	d.tPrev = d.curTimestamp
	d.tDeltaPrev = 0
	d.vPrev = math.Float64bits(d.curValue)

	d.br = 16
	d.bitCount = 0
	d.read++
	return true
}

func (d *Decoder) readNext() bool {
	ts, ok := d.readTimestamp()
	if !ok {
		return false
	}

	val, ok := d.readValueBits()
	if !ok {
		return false
	}

	d.curTimestamp = ts
	d.curValue = val
	d.read++
	return true
}

func (d *Decoder) readTimestamp() (int64, bool) {
	bit, ok := d.readBit()
	if !ok {
		d.setEOF()
		return 0, false
	}

	var dod int64

	if bit == 0 {
		dod = 0
	} else {
		bit, ok = d.readBit()
		if !ok {
			d.setEOF()
			return 0, false
		}
		if bit == 0 {
			// prefix 10: 7-bit value
			v, ok := d.readBits(7)
			if !ok {
				d.setEOF()
				return 0, false
			}
			dod = int64(v) - 63
		} else {
			bit, ok = d.readBit()
			if !ok {
				d.setEOF()
				return 0, false
			}
			if bit == 0 {
				// prefix 110: 9-bit value
				v, ok := d.readBits(9)
				if !ok {
					d.setEOF()
					return 0, false
				}
				dod = int64(v) - 255
			} else {
				bit, ok = d.readBit()
				if !ok {
					d.setEOF()
					return 0, false
				}
				if bit == 0 {
					// prefix 1110: 12-bit value
					v, ok := d.readBits(12)
					if !ok {
						d.setEOF()
						return 0, false
					}
					dod = int64(v) - 2047
				} else {
					// prefix 1111: 64-bit raw dod
					v, ok := d.readBits(64)
					if !ok {
						d.setEOF()
						return 0, false
					}
					dod = int64(v)
				}
			}
		}
	}

	delta := d.tDeltaPrev + dod
	timestamp := d.tPrev + delta
	d.tDeltaPrev = delta
	d.tPrev = timestamp
	return timestamp, true
}

func (d *Decoder) readValueBits() (float64, bool) {
	bit, ok := d.readBit()
	if !ok {
		d.setEOF()
		return 0, false
	}

	if bit == 0 {
		return math.Float64frombits(d.vPrev), true
	}

	bit, ok = d.readBit()
	if !ok {
		d.setEOF()
		return 0, false
	}

	var xor uint64

	if bit == 0 {
		// Use previous leading/trailing zeros window
		sigBits := 64 - int(d.vLeadingZeros) - int(d.vTrailingZeros)
		if sigBits <= 0 {
			d.err = fmt.Errorf("invalid significant bits: %d (leading=%d, trailing=%d)", sigBits, d.vLeadingZeros, d.vTrailingZeros)
			d.done = true
			return 0, false
		}
		v, ok := d.readBits(uint(sigBits))
		if !ok {
			d.setEOF()
			return 0, false
		}
		xor = v << d.vTrailingZeros
	} else {
		// New leading/trailing zeros
		leadV, ok := d.readBits(5)
		if !ok {
			d.setEOF()
			return 0, false
		}
		sigV, ok2 := d.readBits(6)
		if !ok2 {
			d.setEOF()
			return 0, false
		}
		leading := uint8(leadV)
		sigBits := uint8(sigV)
		if sigBits == 0 {
			sigBits = 64
		}
		trailing := 64 - leading - sigBits

		v, ok3 := d.readBits(uint(sigBits))
		if !ok3 {
			d.setEOF()
			return 0, false
		}
		xor = v << trailing

		d.vLeadingZeros = leading
		d.vTrailingZeros = trailing
	}

	vBits := d.vPrev ^ xor
	d.vPrev = vBits
	return math.Float64frombits(vBits), true
}

func (d *Decoder) setEOF() {
	d.done = true
}

func (d *Decoder) readBit() (byte, bool) {
	if d.br >= uint(len(d.buf)) {
		return 0, false
	}
	bit := (d.buf[d.br] >> (7 - d.bitCount)) & 1
	d.bitCount++
	if d.bitCount == 8 {
		d.br++
		d.bitCount = 0
	}
	return bit, true
}

func (d *Decoder) readBits(n uint) (uint64, bool) {
	var val uint64
	for i := uint(0); i < n; i++ {
		bit, ok := d.readBit()
		if !ok {
			return 0, false
		}
		val = (val << 1) | uint64(bit)
	}
	return val, true
}

// encoderPool provides pooled encoders to reduce allocations during hot paths.
var encoderPool = sync.Pool{
	New: func() interface{} {
		return NewEncoder()
	},
}

// GetEncoder retrieves a pooled encoder. Call PutEncoder when done.
func GetEncoder() *Encoder {
	enc := encoderPool.Get().(*Encoder)
	enc.Reset()
	return enc
}

// PutEncoder returns an encoder to the pool.
func PutEncoder(enc *Encoder) {
	encoderPool.Put(enc)
}

// Reset clears the encoder state for reuse.
func (e *Encoder) Reset() {
	e.buf = e.buf[:0]
	e.bw = 0
	e.bitCount = 0
	e.tPrev = 0
	e.tDeltaPrev = 0
	e.vPrev = 0
	e.vLeadingZeros = 255
	e.vTrailingZeros = 0
	e.count = 0
}
