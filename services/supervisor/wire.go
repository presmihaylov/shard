// Package supervisor is the wire between the host and shard-init over vsock: ports, messages, frames and the host client.
package supervisor

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/presmihaylov/shard/models"
)

// The guest ports shard-init listens on. The host is the only client and opens every connection.
const (
	ControlPort uint32 = 5000
	ExecPort    uint32 = 5001
	LogsPort    uint32 = 5002
)

// The kinds a control message carries. The host sends the first four; the guest answers each with done or failure, and sends the rest on its own.
const (
	KindRun       = "run"
	KindSignal    = "signal"
	KindStop      = "stop"
	KindReaddress = "readdress"
	KindDone      = "done"
	KindFailure   = "failure"
	KindState     = "state"
	KindReady     = "ready"
	KindExit      = "exit"
	KindRestarts  = "restarts"
	// KindOOM says the guest hit its memory bound, every guest process is gone, and the VM powers off.
	KindOOM = "oom"
)

// Message is one newline-framed control message; Kind says which of the optional fields it carries.
type Message struct {
	Kind string `json:"kind"`
	// ID numbers a host request, and the guest's done or failure carries it back; an event has none.
	ID int `json:"id,omitempty"`
	// Run is what the entrypoint runs as, sent once on the first control connection.
	Run *RunSpec `json:"run,omitempty"`
	// PID and Signal name one signal to a process shard-init started, TERM or KILL.
	PID    int    `json:"pid,omitempty"`
	Signal string `json:"signal,omitempty"`
	// Address is the new guest address after a fork restored a copy of the source.
	Address *Address `json:"address,omitempty"`
	// Ready says the entrypoint forked; a state replay on a new connection carries it too.
	Ready bool `json:"ready,omitempty"`
	// Exit is how the entrypoint last ended, and Restarts what the restart policy kept.
	Exit     *models.ExitStatus   `json:"exit,omitempty"`
	Restarts *models.RestartCount `json:"restarts,omitempty"`
	// Error is why the guest could not do what the host asked, on the failure that answers the request.
	Error string `json:"error,omitempty"`
}

// RunSpec is what the host resolved for the entrypoint; the guest checks nothing against an image.
type RunSpec struct {
	Argv    []string `json:"argv"`
	Env     []string `json:"env,omitempty"`
	WorkDir string   `json:"workdir,omitempty"`
	// User is uid:gid, and Groups the supplementary set, both resolved on the host; empty keeps root.
	User   string   `json:"user,omitempty"`
	Groups []uint32 `json:"groups,omitempty"`
	// The restart policy, in the shape shard-init takes on its flags on Linux.
	Restart models.RestartPolicy `json:"restart,omitempty"`
	Retries int                  `json:"retries,omitempty"`
	Backoff time.Duration        `json:"backoff,omitempty"`
	Reset   time.Duration        `json:"reset,omitempty"`
	// Trust is the merged CA bundle a fronted guest writes before the entrypoint; a VM has no upper layer a host could plant it in.
	Trust *Trust `json:"trust,omitempty"`
}

// Trust is the image roots plus the proxy CA, at the path the image already reads its roots from.
type Trust struct {
	Path  string `json:"path"`
	Roots []byte `json:"roots"`
}

// Address is one IPv4 address the guest takes on its interface, with the default route behind it and the resolver files that name it.
type Address struct {
	Interface string `json:"interface"`
	IP        string `json:"ip"`
	Prefix    int    `json:"prefix"`
	Gateway   string `json:"gateway,omitempty"`
	// Nameservers go into /etc/resolv.conf and Hostname into /etc/hosts; a VM has no upper layer a host could write them to.
	Nameservers []string `json:"nameservers,omitempty"`
	Hostname    string   `json:"hostname,omitempty"`
}

// ExecHeader opens an exec connection: the command, as the host resolved it, before the frames.
type ExecHeader struct {
	Argv    []string `json:"argv"`
	Env     []string `json:"env,omitempty"`
	WorkDir string   `json:"workdir,omitempty"`
	User    string   `json:"user,omitempty"`
	Groups  []uint32 `json:"groups,omitempty"`
	// TTY gives the command a pseudo terminal the guest allocates; Rows and Cols size it.
	TTY  bool   `json:"tty,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
}

// The streams an exec frame carries in its first byte, the API's numbers; the host sends stdin, its close and resize.
const (
	StreamStdin      byte = 0
	StreamStdout     byte = 1
	StreamStderr     byte = 2
	StreamExit       byte = 3
	StreamStdinClose byte = 4
	StreamStarted    byte = 6
	StreamResize     byte = 7
)

// MaxPayload bounds one frame, so a longer write goes as several and no reader allocates for more.
const MaxPayload = 1 << 20

// frameHeader is the stream byte, three bytes of zero, and the payload length, big endian.
const frameHeader = 8

// StartedFrame is the payload of StreamStarted: the guest pid of the command, for Signal.
type StartedFrame struct {
	PID int `json:"pid"`
}

// ExitFrame is the payload of StreamExit. Error is set when the guest could not start the command, and Code is then the shell code for that refusal.
type ExitFrame struct {
	Code   int    `json:"code"`
	Signal int    `json:"signal"`
	Error  string `json:"error,omitempty"`
}

// ResizeFrame is the payload of StreamResize: the new window of the pseudo terminal.
type ResizeFrame struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// WriteFrame writes payload on stream, as several frames when it is longer than MaxPayload.
func WriteFrame(w io.Writer, stream byte, payload []byte) error {
	for {
		piece := payload
		if len(piece) > MaxPayload {
			piece = piece[:MaxPayload]
		}

		var header [frameHeader]byte
		header[0] = stream
		binary.BigEndian.PutUint32(header[4:], uint32(len(piece))) //nolint:gosec // bounded by MaxPayload above
		if _, err := w.Write(append(header[:], piece...)); err != nil {
			return fmt.Errorf("send a frame of stream %d: %w", stream, err)
		}

		payload = payload[len(piece):]
		if len(payload) == 0 {
			return nil
		}
	}
}

// WriteJSONFrame writes one value on stream, encoded as JSON.
func WriteJSONFrame(w io.Writer, stream byte, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode a frame of stream %d: %w", stream, err)
	}

	return WriteFrame(w, stream, encoded)
}

// ReadFrame reads one frame. It refuses a length over MaxPayload, so a bad peer never makes it allocate.
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var header [frameHeader]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}

	length := binary.BigEndian.Uint32(header[4:])
	if length > MaxPayload {
		return 0, nil, fmt.Errorf("a frame of stream %d claims %d bytes, over the %d bound", header[0], length, MaxPayload)
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, fmt.Errorf("read the %d byte payload of stream %d: %w", length, header[0], err)
	}

	return header[0], payload, nil
}

// WriteMessage frames one control message, or an exec header, as one JSON line.
func WriteMessage(w io.Writer, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode a message: %w", err)
	}

	if _, err := w.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("send a message: %w", err)
	}

	return nil
}

// ReadMessage takes the next JSON line into value. A closed peer reads as io.EOF, unwrapped.
func ReadMessage(r *bufio.Reader, value any) error {
	line, err := r.ReadBytes('\n')
	if errors.Is(err, io.EOF) && len(line) == 0 {
		return io.EOF
	}
	if err != nil {
		return fmt.Errorf("read a message: %w", err)
	}

	return DecodeFrame(line, value)
}

// ReadHeader takes the exec header a byte at a time, so the frames behind it stay in the connection.
func ReadHeader(r io.Reader, value any) error {
	var line []byte
	for {
		var b [1]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return fmt.Errorf("read the exec header: %w", err)
		}
		line = append(line, b[0])
		if b[0] == '\n' {
			return DecodeFrame(line, value)
		}
		if len(line) > MaxPayload {
			return fmt.Errorf("the exec header runs past %d bytes", MaxPayload)
		}
	}
}

// DecodeFrame takes one JSON payload into value.
func DecodeFrame(payload []byte, value any) error {
	if err := json.Unmarshal(payload, value); err != nil {
		return fmt.Errorf("decode a frame: %w", err)
	}

	return nil
}
