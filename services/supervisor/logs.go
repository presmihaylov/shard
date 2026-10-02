package supervisor

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"

	"github.com/presmihaylov/shard/pkg/store"
)

// LogSink is where the guest's output lands. Resume answers a guest that holds output bytes [from, to) with the one to send from.
type LogSink interface {
	io.Writer
	Resume(from, to uint64) (uint64, error)
}

// Logs lands the entrypoint's output in sink until the guest, or ctx, ends the connection.
func Logs(ctx context.Context, dial Dialer, sink LogSink) error {
	conn, err := dial(ctx, LogsPort)
	if err != nil {
		return fmt.Errorf("open the logs connection: %w", err)
	}
	defer conn.Close()

	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	if err := followLogs(conn, sink); err != nil && ctx.Err() == nil {
		return err
	}

	return nil
}

// followLogs reads the output offsets the guest holds, answers where to resume, then acks each write so the guest lets it go.
func followLogs(conn net.Conn, sink LogSink) error {
	var held [2]uint64
	if err := binary.Read(conn, binary.BigEndian, &held); err != nil {
		return fmt.Errorf("read the output the guest holds: %w", err)
	}
	at, err := sink.Resume(held[0], held[1])
	if err != nil {
		return fmt.Errorf("resume the guest logs: %w", err)
	}
	if err := binary.Write(conn, binary.BigEndian, at); err != nil {
		return fmt.Errorf("resume the guest logs: %w", err)
	}

	buf := make([]byte, 32<<10)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if _, err := sink.Write(buf[:n]); err != nil {
				return fmt.Errorf("follow the guest logs: %w", err)
			}
			at += uint64(n)
			if err := binary.Write(conn, binary.BigEndian, at); err != nil {
				return fmt.Errorf("ack the guest logs: %w", err)
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("follow the guest logs: %w", err)
		}
	}
}

// FileLog lands the guest's output in a file, and keeps beside it which output byte the file holds where, so a host that comes back lands none twice.
type FileLog struct {
	File   *os.File
	Cursor string
	// Err is the first failure of the file or the cursor, which the connection's own errors would otherwise hide.
	Err error
}

// logCursor says the log file's byte at Offset is the guest's output byte at Output.
type logCursor struct {
	Offset uint64 `json:"offset"`
	Output uint64 `json:"output"`
}

func (l *FileLog) Write(b []byte) (int, error) {
	n, err := l.File.Write(b)

	return n, l.fail(err)
}

// Resume sends the guest on from the output byte the file ends at, or from the oldest it holds when the cursor cannot place it.
func (l *FileLog) Resume(from, to uint64) (uint64, error) {
	info, err := l.File.Stat()
	if err != nil {
		return 0, l.fail(fmt.Errorf("measure the log: %w", err))
	}
	size := uint64(info.Size()) //nolint:gosec // a file size is never negative
	c, found, err := readCursor(l.Cursor)
	if err != nil {
		return 0, l.fail(err)
	}
	if found && size >= c.Offset {
		at := c.Output + size - c.Offset
		if at >= from && at <= to {
			return at, nil
		}
	}

	// A boot the cursor has not seen, or one it no longer matches, is a guest this file holds nothing of yet.
	encoded, err := json.Marshal(logCursor{Offset: size, Output: from})
	if err != nil {
		return 0, l.fail(fmt.Errorf("marshal the log cursor: %w", err))
	}
	if err := store.WriteFile(l.Cursor, encoded, 0o600); err != nil {
		return 0, l.fail(fmt.Errorf("write the log cursor: %w", err))
	}

	return from, nil
}

func (l *FileLog) fail(err error) error {
	if err != nil && l.Err == nil {
		l.Err = err
	}

	return err
}

func readCursor(path string) (logCursor, bool, error) {
	var c logCursor
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, false, nil
	}
	if err != nil {
		return c, false, fmt.Errorf("read the log cursor: %w", err)
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, false, fmt.Errorf("parse the log cursor %s: %w", path, err)
	}

	return c, true, nil
}
