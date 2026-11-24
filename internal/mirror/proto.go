package mirror

// proto.go is the one-way wire protocol between the in-container watcher
// (cmd/ahjo-mirror, untrusted) and the host-side writer (Receiver, trusted).
// The watcher writes frames to its stdout; the host reads them through
// `incus exec`. Nothing flows back.
//
// Frame = type (1 byte) + length (uint32 big-endian) + payload (length bytes).

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

const (
	// ProtoVersion is bumped on any incompatible change to the frame set.
	ProtoVersion = 1
	// MaxFramePayload bounds a single frame; the host rejects anything larger
	// before allocating for it.
	MaxFramePayload = 1 << 20
	// DataChunkSize is the watcher's read/emit granularity for file bodies.
	DataChunkSize = 256 << 10
	// MaxFileSize is the per-file cap. The watcher skips larger files and the
	// host aborts a file whose received bytes exceed it.
	MaxFileSize int64 = 512 << 20
)

// FrameType identifies a frame.
type FrameType byte

const (
	FrameHello         FrameType = 'H' // JSON Hello; must be first
	FrameMkdir         FrameType = 'M' // JSON MkdirMsg
	FrameFile          FrameType = 'F' // JSON FileMsg; opens a file
	FrameData          FrameType = 'D' // raw chunk of the open file
	FrameEnd           FrameType = 'E' // JSON EndMsg; closes the open file
	FrameSymlink       FrameType = 'L' // JSON SymlinkMsg
	FrameBootstrapDone FrameType = 'B' // empty
	FramePing          FrameType = 'P' // empty heartbeat
)

// Hello is the first frame of every session.
type Hello struct {
	Proto      int    `json:"proto"`
	Version    string `json:"version"`
	NoSkiplist bool   `json:"no_skiplist"`
}

// MkdirMsg announces a directory that exists in the source.
type MkdirMsg struct {
	Path string `json:"path"`
}

// FileMsg opens a regular file. Size is advisory (used only for the bootstrap
// fast-skip); the host counts the bytes it actually receives.
type FileMsg struct {
	Path      string `json:"path"`
	Mode      uint32 `json:"mode"`
	MtimeNs   int64  `json:"mtime_ns"`
	Size      int64  `json:"size"`
	Bootstrap bool   `json:"bootstrap"`
}

// EndMsg closes the open file; OK=false means "discard what was sent".
type EndMsg struct {
	OK bool `json:"ok"`
}

// SymlinkMsg announces a symlink.
type SymlinkMsg struct {
	Path      string `json:"path"`
	Target    string `json:"target"`
	Bootstrap bool   `json:"bootstrap"`
}

// FrameWriter serializes frames onto w. Safe for concurrent use, so the
// watcher's heartbeat goroutine can interleave pings with the main loop.
type FrameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// NewFrameWriter wraps w.
func NewFrameWriter(w io.Writer) *FrameWriter { return &FrameWriter{w: w} }

// Write emits one frame.
func (fw *FrameWriter) Write(t FrameType, payload []byte) error {
	if len(payload) > MaxFramePayload {
		return fmt.Errorf("frame %c payload %d exceeds %d", t, len(payload), MaxFramePayload)
	}
	var hdr [5]byte
	hdr[0] = byte(t)
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if _, err := fw.w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := fw.w.Write(payload)
	return err
}

// WriteJSON emits a frame whose payload is v as JSON.
func (fw *FrameWriter) WriteJSON(t FrameType, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return fw.Write(t, b)
}

// FrameReader decodes frames from r.
type FrameReader struct {
	r   io.Reader
	buf []byte
}

// NewFrameReader wraps r.
func NewFrameReader(r io.Reader) *FrameReader { return &FrameReader{r: r} }

// Next returns the next frame. The payload slice is only valid until the next
// call. A clean end of stream between frames returns io.EOF; a stream cut
// mid-frame returns io.ErrUnexpectedEOF.
func (fr *FrameReader) Next() (FrameType, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(fr.r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxFramePayload {
		return 0, nil, fmt.Errorf("frame %q payload %d exceeds %d", hdr[0], n, MaxFramePayload)
	}
	if cap(fr.buf) < int(n) {
		fr.buf = make([]byte, n)
	}
	fr.buf = fr.buf[:n]
	if _, err := io.ReadFull(fr.r, fr.buf); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return FrameType(hdr[0]), fr.buf, nil
}
