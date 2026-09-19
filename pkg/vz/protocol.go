package vz

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// request is one verb over the shim socket; connect turns the connection into the vsock stream after its reply.
type request struct {
	Verb string `json:"verb"`
	Path string `json:"path,omitempty"`
	Port uint32 `json:"port,omitempty"`
}

type response struct {
	Error     string `json:"error,omitempty"`
	State     State  `json:"state,omitempty"`
	PID       int    `json:"pid,omitempty"`
	MachineID string `json:"machine_id,omitempty"`
}

// maxFrame bounds a frame so a stray writer on the socket cannot make either side allocate at will.
const maxFrame = 1 << 16

// A frame is a 4-byte big-endian length and that many bytes of JSON.
func writeFrame(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal the frame: %w", err)
	}
	n := len(body)
	if n > maxFrame {
		return fmt.Errorf("the frame is %d bytes, over the %d limit", n, maxFrame)
	}

	frame := binary.BigEndian.AppendUint32(make([]byte, 0, 4+n), uint32(n)) //nolint:gosec // bounded by maxFrame just above
	if _, err := w.Write(append(frame, body...)); err != nil {
		return fmt.Errorf("write the frame: %w", err)
	}

	return nil
}

func readFrame(r io.Reader, v any) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return fmt.Errorf("read the frame length: %w", err)
	}

	n := binary.BigEndian.Uint32(header[:])
	if n > maxFrame {
		return fmt.Errorf("frame of %d bytes is over the %d limit", n, maxFrame)
	}

	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return fmt.Errorf("read the frame: %w", err)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("decode the frame: %w", err)
	}

	return nil
}

func (r response) err() error {
	if r.Error == "" {
		return nil
	}

	return errors.New(r.Error)
}
