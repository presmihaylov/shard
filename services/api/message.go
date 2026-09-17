package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/coder/websocket"

	"github.com/presmihaylov/shard/models"
)

// The streams a message carries, in its first byte. The client sends stdin and stdin closed, the daemon the rest.
const (
	StreamStdin      byte = 0
	StreamStdout     byte = 1
	StreamStderr     byte = 2
	StreamExit       byte = 3
	StreamStdinClose byte = 4
	StreamFailure    byte = 5
)

// MaxPayload bounds one message, so a longer write goes as several and no reader allocates for more.
const MaxPayload = 1 << 20

// ExitMessage is the payload of StreamExit on an exec. Error is set when the sandbox could not start the command.
type ExitMessage struct {
	Code   int    `json:"code"`
	Signal int    `json:"signal"`
	Error  string `json:"error,omitempty"`
}

// EndMessage is the payload of StreamExit on a log follow: why the daemon stopped following.
type EndMessage struct {
	Reason string `json:"reason"`
}

// FailureMessage is the payload of StreamFailure, nested under error like every other error body.
type FailureMessage struct {
	Error FailureError `json:"error"`
}

// FailureError is the code and the message a StreamFailure carries.
type FailureError struct {
	Code    models.Code `json:"code"`
	Message string      `json:"message"`
}

// Send writes payload on stream, as several messages when it is longer than MaxPayload.
func Send(ctx context.Context, conn *websocket.Conn, stream byte, payload []byte) error {
	for {
		piece := payload
		if len(piece) > MaxPayload {
			piece = piece[:MaxPayload]
		}

		if err := conn.Write(ctx, websocket.MessageBinary, append([]byte{stream}, piece...)); err != nil {
			return fmt.Errorf("send a message of stream %d: %w", stream, err)
		}

		payload = payload[len(piece):]
		if len(payload) == 0 {
			return nil
		}
	}
}

// Receive reads one message and splits the stream off its payload. A text or empty message is no stream at all.
func Receive(ctx context.Context, conn *websocket.Conn) (byte, []byte, error) {
	kind, data, err := conn.Read(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("receive a message: %w", err)
	}
	if kind != websocket.MessageBinary || len(data) == 0 {
		return 0, nil, errors.New("received a message that names no stream")
	}

	return data[0], data[1:], nil
}
