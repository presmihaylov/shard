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

// LogsVersion is the logs port protocol a guest names in its state; no raw output can forge a field of the control stream.
const LogsVersion = 1

// The kinds a control message carries. The host sends the first seven; the guest answers each with done or failure, and sends the rest on its own.
const (
	KindRun       = "run"
	KindSignal    = "signal"
	KindStop      = "stop"
	KindReaddress = "readdress"
	KindReseed    = "reseed"
	KindFreeze    = "freeze"
	KindThaw      = "thaw"
	KindDone      = "done"
	KindFailure   = "failure"
	KindState     = "state"
	KindReady     = "ready"
	KindExit      = "exit"
	KindRestarts  = "restarts"
	// KindOOM says the guest hit its memory bound, every guest process is gone, and the VM powers off.
	KindOOM = "oom"
	// KindSupervisorFailed is shard-init's own death: a VM halts when PID 1 exits, so the 125 goes over the wire first.
	KindSupervisorFailed = "supervisor-failed"
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
	// Seed is host entropy for a restored guest's crng, which woke with the key of every other restore of the same save.
	Seed []byte `json:"seed,omitempty"`
	// Ready says the entrypoint forked; a state replay on a new connection carries it too.
	Ready bool `json:"ready,omitempty"`
	// Exit is how the entrypoint last ended, and Restarts what the restart policy kept.
	Exit     *models.ExitStatus   `json:"exit,omitempty"`
	Restarts *models.RestartCount `json:"restarts,omitempty"`
	// OOM on a state replay says the bound took every guest process while no host was attached to hear it.
	OOM bool `json:"oom,omitempty"`
	// Frozen on a state replay says the guest's root still holds its writes, as a pause left it.
	Frozen bool `json:"frozen,omitempty"`
	// Logs on a state replay is the logs port protocol the guest speaks; zero is a guest from before it, which sends raw output and reads no acks.
	Logs int `json:"logs,omitempty"`
	// FreezesOverlay on a state replay says the guest freezes an overlay root by its upper; a guest from before it fails every freeze on one.
	FreezesOverlay bool `json:"freezes_overlay,omitempty"`
	// Error is why the guest could not do what the host asked, on the failure that answers the request, or why the supervisor gave up.
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
	// MAC replaces the hardware address of the interface; a fork restored from another guest's memory still carries that guest's.
	MAC     string `json:"mac,omitempty"`
	IP      string `json:"ip"`
	Prefix  int    `json:"prefix"`
	Gateway string `json:"gateway,omitempty"`
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
	// Lookup says User is what the caller named, for the guest to resolve against its live passwd; an older guest refuses a name.
	Lookup bool `json:"lookup,omitempty"`
	// TTY gives the command a pseudo terminal the guest allocates; Rows and Cols size it.
	TTY  bool   `json:"tty,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
}

// The streams an exec frame carries in its first byte, the API's numbers; the host sends stdin, its close, resize and cancel.
const (
	StreamStdin      byte = 0
	StreamStdout     byte = 1
	StreamStderr     byte = 2
	StreamExit       byte = 3
	StreamStdinClose byte = 4
	StreamStarted    byte = 6
	StreamResize     byte = 7
	StreamCancel     byte = 8
)

// MaxPayload bounds one frame, so a longer write goes as several and no reader allocates for more.
const MaxPayload = 1 << 20

// ErrMessageTooLong is a control line past MaxPayload; the reader refuses it before it holds more (SHARD-340).
var ErrMessageTooLong = errors.New("a message runs past the 1 MiB bound")

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
	if len(encoded) > MaxPayload {
		return fmt.Errorf("send a message: %w", ErrMessageTooLong)
	}

	if _, err := w.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("send a message: %w", err)
	}

	return nil
}

// ReadMessage takes the next JSON line into value, up to MaxPayload before its newline. A closed peer reads as io.EOF, unwrapped.
func ReadMessage(r *bufio.Reader, value any) error {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(line)+len(chunk) > MaxPayload+1 {
			return fmt.Errorf("read a message: %w", ErrMessageTooLong)
		}
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(line) == 0 {
			return io.EOF
		}
		if err != nil {
			return fmt.Errorf("read a message: %w", err)
		}

		return DecodeFrame(line, value)
	}
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
