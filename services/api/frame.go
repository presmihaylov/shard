package api

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// The streams one exec carries. The client sends Stdin and StdinClose, the daemon the other three.
const (
	StreamStdin      byte = 0
	StreamStdout     byte = 1
	StreamStderr     byte = 2
	StreamExit       byte = 3
	StreamStdinClose byte = 4
	StreamError      byte = 5
)

// FrameHeaderSize is the stream byte, three zero bytes, and the payload length as a big-endian uint32.
const FrameHeaderSize = 8

// MaxFrameSize bounds one payload, so a length nobody meant cannot make the reader allocate for it.
const MaxFrameSize = 1 << 20

// ExecIDHeader carries the id of the exec on the 101, which is what a resize names.
const ExecIDHeader = "X-Shard-Exec-Id"

// WriteFrame writes one frame of stream. A payload longer than MaxFrameSize goes as several frames.
func WriteFrame(w io.Writer, stream byte, payload []byte) error {
	for {
		piece := payload
		if len(piece) > MaxFrameSize {
			piece = piece[:MaxFrameSize]
		}

		header := make([]byte, FrameHeaderSize, FrameHeaderSize+len(piece))
		header[0] = stream
		binary.BigEndian.PutUint32(header[4:], uint32(len(piece))) //nolint:gosec // G115: a piece is at most MaxFrameSize, which fits a uint32

		if _, err := w.Write(append(header, piece...)); err != nil {
			return fmt.Errorf("write a frame of stream %d: %w", stream, err)
		}

		payload = payload[len(piece):]
		if len(payload) == 0 {
			return nil
		}
	}
}

// ReadFrame reads one frame. It reports io.EOF when the connection ended between two frames.
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var header [FrameHeaderSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return 0, nil, io.EOF
		}

		return 0, nil, fmt.Errorf("read a frame header: %w", err)
	}

	length := binary.BigEndian.Uint32(header[4:])
	if length > MaxFrameSize {
		return 0, nil, fmt.Errorf("a frame of stream %d says it is %d bytes, and no frame is over %d", header[0], length, MaxFrameSize)
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, fmt.Errorf("read the %d bytes of a frame of stream %d: %w", length, header[0], err)
	}

	return header[0], payload, nil
}

// FrameWriter serializes the frames of one exec: the guest's stdout and stderr arrive on two goroutines.
type FrameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func NewFrameWriter(w io.Writer) *FrameWriter { return &FrameWriter{w: w} }

func (f *FrameWriter) Write(stream byte, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	return WriteFrame(f.w, stream, payload)
}
