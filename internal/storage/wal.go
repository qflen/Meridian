// Package storage implements the Meridian time-series storage engine including
// the write-ahead log, in-memory head block, and persistent compressed blocks.
package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	walSegmentMaxSize = 128 * 1024 * 1024 // 128 MB
	walFrameHeader    = 8                  // 4 bytes CRC + 4 bytes length
	walAlignment      = 8                  // pad frames to 8-byte boundary

	// WAL entry type markers.
	walEntrySeries  byte = 0x01
	walEntrySamples byte = 0x02
)

// Sample represents a single timestamped data point for a series.
type Sample struct {
	SeriesID  uint64
	Timestamp int64
	Value     float64
}

// WALHandler processes replayed WAL entries.
type WALHandler interface {
	HandleSeries(id uint64, name string, labels map[string]string) error
	HandleSamples(samples []Sample) error
}

// WAL is an append-only write-ahead log with CRC32-framed entries.
type WAL struct {
	mu  sync.Mutex
	dir string

	segment     *os.File
	segmentSize int64
	segmentSeq  int
}

// OpenWAL opens or creates a WAL in the given directory.
func OpenWAL(dir string) (*WAL, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create WAL dir: %w", err)
	}

	w := &WAL{dir: dir}
	// Find the highest existing segment number
	segs, err := w.listSegments()
	if err != nil {
		return nil, err
	}
	if len(segs) > 0 {
		w.segmentSeq = segs[len(segs)-1].seq
	}

	if err := w.rotateSegment(); err != nil {
		return nil, err
	}

	return w, nil
}

// LogSeries writes a series definition to the WAL.
func (w *WAL) LogSeries(id uint64, name string, labels map[string]string) error {
	// Defensive guard: every length-prefixed field is a uint16 on disk, so reject
	// anything that would overflow it rather than silently truncating the frame.
	// Ingest validation rejects these earlier; this keeps the WAL format honest even
	// if a future caller skips that.
	if len(name) > maxFieldLen {
		return fmt.Errorf("WAL: series name length %d exceeds %d", len(name), maxFieldLen)
	}
	if len(labels) > maxFieldLen {
		return fmt.Errorf("WAL: label count %d exceeds %d", len(labels), maxFieldLen)
	}
	for k, v := range labels {
		if len(k) > maxFieldLen {
			return fmt.Errorf("WAL: label name length %d exceeds %d", len(k), maxFieldLen)
		}
		if len(v) > maxFieldLen {
			return fmt.Errorf("WAL: label value length %d exceeds %d", len(v), maxFieldLen)
		}
	}

	// Encode: type(1) + seriesID(8) + nameLen(2) + name + numLabels(2) + labels
	size := 1 + 8 + 2 + len(name) + 2
	for k, v := range labels {
		size += 2 + len(k) + 2 + len(v)
	}

	buf := make([]byte, size)
	off := 0
	buf[off] = walEntrySeries
	off++
	binary.LittleEndian.PutUint64(buf[off:], id)
	off += 8
	binary.LittleEndian.PutUint16(buf[off:], uint16(len(name)))
	off += 2
	copy(buf[off:], name)
	off += len(name)
	binary.LittleEndian.PutUint16(buf[off:], uint16(len(labels)))
	off += 2
	for k, v := range labels {
		binary.LittleEndian.PutUint16(buf[off:], uint16(len(k)))
		off += 2
		copy(buf[off:], k)
		off += len(k)
		binary.LittleEndian.PutUint16(buf[off:], uint16(len(v)))
		off += 2
		copy(buf[off:], v)
		off += len(v)
	}

	return w.writeFrame(buf)
}

// LogSamples writes a batch of samples to the WAL.
func (w *WAL) LogSamples(samples []Sample) error {
	if len(samples) == 0 {
		return nil
	}

	// Encode: type(1) + count(4) + (seriesID(8) + ts(8) + val(8)) × N
	size := 1 + 4 + len(samples)*24
	buf := make([]byte, size)
	off := 0
	buf[off] = walEntrySamples
	off++
	binary.LittleEndian.PutUint32(buf[off:], uint32(len(samples)))
	off += 4
	for _, s := range samples {
		binary.LittleEndian.PutUint64(buf[off:], s.SeriesID)
		off += 8
		binary.LittleEndian.PutUint64(buf[off:], uint64(s.Timestamp))
		off += 8
		binary.LittleEndian.PutUint64(buf[off:], math.Float64bits(s.Value))
		off += 8
	}

	return w.writeFrame(buf)
}

// Replay reads all WAL segments in order and calls the handler for each entry.
// Corrupt or partial frames are skipped; recovery resyncs to the next valid frame.
func (w *WAL) Replay(handler WALHandler) error {
	return w.ReplayFrom(0, handler)
}

// ReplayFrom reads WAL segments with sequence strictly greater than afterSeq in
// order, calling the handler for each entry. Segments at or below afterSeq are
// already durably covered by a persisted block (their data is in the block), so
// skipping them is what prevents double-counting on recovery. Segment sequences
// start at 1, so afterSeq==0 replays everything.
func (w *WAL) ReplayFrom(afterSeq int, handler WALHandler) error {
	segs, err := w.listSegments()
	if err != nil {
		return err
	}

	for _, seg := range segs {
		if seg.seq <= afterSeq {
			continue
		}
		if err := w.replaySegment(seg.path, handler); err != nil {
			return fmt.Errorf("replay segment %s: %w", seg.path, err)
		}
	}
	return nil
}

// Rotate seals the current segment and starts a new one. It returns the sequence
// number of the highest sealed segment: every frame written before this call lives
// in a segment with seq <= the returned value, and every subsequent write lands in
// a segment with seq greater than it. The flush path uses this as a block's WAL
// low-water-mark — the cut between data that belongs to the flushed block and data
// that belongs to the fresh head.
func (w *WAL) Rotate() (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	sealed := w.segmentSeq
	if err := w.rotateSegmentLocked(); err != nil {
		return 0, err
	}
	return sealed, nil
}

// RemoveSegmentsThrough deletes every WAL segment with sequence <= seq. It is
// best-effort cleanup run after a block durably covers those segments; the open
// segment (always seq > any sealed low-water-mark) is never removed. Failures are
// aggregated and returned but are non-fatal: replay skips covered segments by
// low-water-mark whether or not they were deleted.
func (w *WAL) RemoveSegmentsThrough(seq int) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	segs, err := w.listSegments()
	if err != nil {
		return err
	}
	var errs []error
	for _, s := range segs {
		if s.seq <= seq && s.seq != w.segmentSeq {
			if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// Truncate deletes all existing WAL segments and starts fresh. The flush path uses
// Rotate + RemoveSegmentsThrough instead; Truncate remains for explicit resets.
func (w *WAL) Truncate() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	var errs []error
	if w.segment != nil {
		if err := w.segment.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close segment: %w", err))
		}
		w.segment = nil
	}

	segs, err := w.listSegments()
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, seg := range segs {
		if err := os.Remove(seg.path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}

	w.segmentSeq++
	if err := w.openSegment(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Close flushes and closes the WAL. Sync and Close errors are aggregated, and the
// segment is always closed (even if Sync fails) so the file descriptor never leaks.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.segment == nil {
		return nil
	}
	var errs []error
	if err := w.segment.Sync(); err != nil {
		errs = append(errs, fmt.Errorf("sync: %w", err))
	}
	if err := w.segment.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close: %w", err))
	}
	w.segment = nil
	return errors.Join(errs...)
}

// Size returns the total size of all WAL segments in bytes.
func (w *WAL) Size() int64 {
	segs, _ := w.listSegments()
	var total int64
	for _, seg := range segs {
		info, err := os.Stat(seg.path)
		if err == nil {
			total += info.Size()
		}
	}
	return total
}

func (w *WAL) writeFrame(payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.segment == nil {
		return fmt.Errorf("WAL: no open segment")
	}

	// Frame: CRC32(4) + Length(4) + Payload + Padding
	frameLen := walFrameHeader + len(payload)
	padded := (frameLen + walAlignment - 1) / walAlignment * walAlignment
	frame := make([]byte, padded)

	checksum := crc32.ChecksumIEEE(payload)
	binary.LittleEndian.PutUint32(frame[0:4], checksum)
	binary.LittleEndian.PutUint32(frame[4:8], uint32(len(payload)))
	copy(frame[walFrameHeader:], payload)

	if w.segmentSize+int64(len(frame)) > walSegmentMaxSize {
		if err := w.rotateSegmentLocked(); err != nil {
			return err
		}
	}

	n, err := w.segment.Write(frame)
	if err != nil {
		return fmt.Errorf("WAL write: %w", err)
	}
	w.segmentSize += int64(n)

	if err := w.segment.Sync(); err != nil {
		return fmt.Errorf("WAL sync: %w", err)
	}
	return nil
}

func (w *WAL) rotateSegment() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.rotateSegmentLocked()
}

func (w *WAL) rotateSegmentLocked() error {
	old := w.segment
	oldSeq := w.segmentSeq

	// Open the new segment first. openSegment only mutates w.segment/w.segmentSize on
	// success, so a failed open leaves the current segment intact — the WAL is never
	// left "open" with a nil segment.
	w.segmentSeq++
	if err := w.openSegment(); err != nil {
		w.segmentSeq = oldSeq
		return fmt.Errorf("rotate WAL segment: %w", err)
	}

	// Retire the old segment. Every frame already fsynced on write, so these errors
	// are non-fatal to durability; aggregate and surface them rather than dropping.
	var errs []error
	if old != nil {
		if err := old.Sync(); err != nil {
			errs = append(errs, fmt.Errorf("sync segment %06d: %w", oldSeq, err))
		}
		if err := old.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close segment %06d: %w", oldSeq, err))
		}
	}
	return errors.Join(errs...)
}

func (w *WAL) openSegment() error {
	path := filepath.Join(w.dir, fmt.Sprintf("segment-%06d", w.segmentSeq))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open WAL segment: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.segment = f
	w.segmentSize = info.Size()
	return nil
}

type walSegment struct {
	path string
	seq  int
}

func (w *WAL) listSegments() ([]walSegment, error) {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var segs []walSegment
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "segment-") {
			continue
		}
		var seq int
		if _, err := fmt.Sscanf(e.Name(), "segment-%06d", &seq); err != nil {
			continue
		}
		segs = append(segs, walSegment{
			path: filepath.Join(w.dir, e.Name()),
			seq:  seq,
		})
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].seq < segs[j].seq })
	return segs, nil
}

// replaySegment reads a segment frame-by-frame, recovering as much as possible from
// a corrupt or torn segment. Frames are 8-byte aligned, so on an implausible length
// field, a frame that runs past the data, or a CRC mismatch, recovery re-anchors at
// the next 8-byte boundary and keeps scanning — a single bad frame no longer
// discards every valid frame that follows it.
func (w *WAL) replaySegment(path string, handler WALHandler) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	off := 0
	for off+walFrameHeader <= len(data) {
		expectedCRC := binary.LittleEndian.Uint32(data[off : off+4])
		payloadLen := binary.LittleEndian.Uint32(data[off+4 : off+8])

		frameLen := walFrameHeader + int(payloadLen)
		padded := (frameLen + walAlignment - 1) / walAlignment * walAlignment

		// Implausible length, or a frame that would run past the segment (a torn tail
		// or a corrupt length field): re-anchor at the next aligned boundary.
		if payloadLen == 0 || payloadLen > walSegmentMaxSize || off+padded > len(data) {
			off += walAlignment
			continue
		}

		payload := data[off+walFrameHeader : off+walFrameHeader+int(payloadLen)]
		if crc32.ChecksumIEEE(payload) != expectedCRC {
			// Corrupt frame: a flipped payload bit, or a corrupt length that happened to
			// fit. Skip one alignment unit and keep scanning for the next valid frame.
			off += walAlignment
			continue
		}

		if err := w.decodeEntry(payload, handler); err != nil {
			log.Printf("WAL: error decoding entry at offset %d: %v", off, err)
		}
		off += padded
	}
	return nil
}

func (w *WAL) decodeEntry(payload []byte, handler WALHandler) error {
	if len(payload) == 0 {
		return fmt.Errorf("empty payload")
	}

	switch payload[0] {
	case walEntrySeries:
		return w.decodeSeries(payload[1:], handler)
	case walEntrySamples:
		return w.decodeSamples(payload[1:], handler)
	default:
		return fmt.Errorf("unknown entry type: %x", payload[0])
	}
}

func (w *WAL) decodeSeries(data []byte, handler WALHandler) error {
	if len(data) < 12 {
		return fmt.Errorf("series entry too short")
	}
	off := 0
	id := binary.LittleEndian.Uint64(data[off:])
	off += 8
	nameLen := int(binary.LittleEndian.Uint16(data[off:]))
	off += 2
	if off+nameLen > len(data) {
		return fmt.Errorf("series name truncated")
	}
	name := string(data[off : off+nameLen])
	off += nameLen

	if off+2 > len(data) {
		return fmt.Errorf("series labels truncated")
	}
	numLabels := int(binary.LittleEndian.Uint16(data[off:]))
	off += 2

	labels := make(map[string]string, numLabels)
	for i := 0; i < numLabels; i++ {
		if off+2 > len(data) {
			return fmt.Errorf("label key truncated")
		}
		kLen := int(binary.LittleEndian.Uint16(data[off:]))
		off += 2
		if off+kLen > len(data) {
			return fmt.Errorf("label key data truncated")
		}
		k := string(data[off : off+kLen])
		off += kLen

		if off+2 > len(data) {
			return fmt.Errorf("label value truncated")
		}
		vLen := int(binary.LittleEndian.Uint16(data[off:]))
		off += 2
		if off+vLen > len(data) {
			return fmt.Errorf("label value data truncated")
		}
		v := string(data[off : off+vLen])
		off += vLen

		labels[k] = v
	}

	return handler.HandleSeries(id, name, labels)
}

func (w *WAL) decodeSamples(data []byte, handler WALHandler) error {
	if len(data) < 4 {
		return fmt.Errorf("samples entry too short")
	}
	count := int(binary.LittleEndian.Uint32(data[0:4]))
	off := 4
	// Validate count against the available bytes BEFORE multiplying, so count*24
	// cannot overflow a 32-bit int on 32-bit builds.
	if maxCount := (len(data) - off) / 24; count > maxCount {
		return fmt.Errorf("samples data truncated: count %d exceeds capacity %d", count, maxCount)
	}

	samples := make([]Sample, count)
	for i := 0; i < count; i++ {
		samples[i].SeriesID = binary.LittleEndian.Uint64(data[off:])
		off += 8
		samples[i].Timestamp = int64(binary.LittleEndian.Uint64(data[off:]))
		off += 8
		samples[i].Value = math.Float64frombits(binary.LittleEndian.Uint64(data[off:]))
		off += 8
	}

	return handler.HandleSamples(samples)
}
