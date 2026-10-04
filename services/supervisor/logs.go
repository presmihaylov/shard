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
	"time"

	"github.com/presmihaylov/shard/pkg/logfile"
	"github.com/presmihaylov/shard/pkg/store"
)

// LogSink is where the guest's output lands. Resume answers a guest that holds output bytes [from, to) with the one to send from.
type LogSink interface {
	io.Writer
	Resume(from, to uint64) (uint64, error)
}

// Logs lands the entrypoint's output in sink until the guest, or ctx, ends the connection; version is the one the guest's state named.
func Logs(ctx context.Context, dial Dialer, sink LogSink, version int) error {
	conn, err := dial(ctx, LogsPort)
	if err != nil {
		return fmt.Errorf("open the logs connection: %w", err)
	}
	defer conn.Close()

	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	if err := followLogs(conn, sink, version); err != nil && ctx.Err() == nil {
		return err
	}

	return nil
}

// ErrLogsVersion is a guest whose logs protocol this host cannot read; a redial meets the same guest again.
var ErrLogsVersion = errors.New("unknown guest logs version")

// followLogs reads the output offsets the guest holds, answers where to resume, then acks each write so the guest lets it go.
func followLogs(conn net.Conn, sink LogSink, version int) error {
	if version == 0 {
		// An older guest sends raw output with no header and reads no acks.
		return pump(conn, sink, nil, 0)
	}
	if version != LogsVersion {
		return fmt.Errorf("%w %d: this host reads %d", ErrLogsVersion, version, LogsVersion)
	}

	var held [2]uint64
	if err := binary.Read(conn, binary.BigEndian, &held); err != nil {
		return fmt.Errorf("read the output the guest holds: %w", err)
	}
	at, err := sink.Resume(held[0], held[1])
	if err != nil {
		return errors.Join(fmt.Errorf("resume the guest logs: %w", err), stopLogs(conn))
	}
	if err := binary.Write(conn, binary.BigEndian, at); err != nil {
		return fmt.Errorf("resume the guest logs: %w", err)
	}

	return pump(conn, sink, conn, at)
}

// LogsHeader is what a guest sends first on the logs port: the output bytes [from, to) it holds.
func LogsHeader(from, to uint64) []byte {
	return binary.BigEndian.AppendUint64(binary.BigEndian.AppendUint64(nil, from), to)
}

// pump lands what r carries in sink and, when ack is set, answers each write with the output offset after it.
func pump(r io.Reader, sink io.Writer, ack net.Conn, at uint64) error {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, err := sink.Write(buf[:n]); err != nil {
				return errors.Join(fmt.Errorf("follow the guest logs: %w", err), stopLogs(ack))
			}
			at += uint64(n)
			if ack != nil {
				if err := binary.Write(ack, binary.BigEndian, at); err != nil {
					return fmt.Errorf("ack the guest logs: %w", err)
				}
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

// stopGrace bounds the word to a guest that its log stopped, and the wait for its hang-up.
const stopGrace = 2 * time.Second

// stopLogs tells a guest that reads acks the log stopped, then waits for its hang-up, so the close cannot drop the word in flight.
func stopLogs(conn net.Conn) error {
	if conn == nil {
		return nil
	}
	if err := conn.SetDeadline(time.Now().Add(stopGrace)); err != nil {
		return fmt.Errorf("tell the guest the log stopped: %w", err)
	}
	if err := binary.Write(conn, binary.BigEndian, LogsStopped); err != nil {
		return fmt.Errorf("tell the guest the log stopped: %w", err)
	}
	if _, err := io.Copy(io.Discard, conn); err != nil {
		return fmt.Errorf("wait for the guest to hang up the logs: %w", err)
	}

	return nil
}

// MaxLog is the most one log file holds before it is rotated, and one rotated file is kept behind it.
const MaxLog = 16 << 20

// FileLog lands the guest's output in a file, and keeps beside it which output byte the file holds where, so a host that comes back lands none twice.
type FileLog struct {
	File   *os.File
	Cursor string
	// Max is the most the file holds: a write that would take it past Max renames it to the rotated file first.
	Max int64
	// Err is the first failure of the file or the cursor, which the connection's own errors would otherwise hide.
	Err error
}

// logCursor says the log file's byte at Offset is the guest's output byte at Output.
type logCursor struct {
	Offset uint64 `json:"offset"`
	Output uint64 `json:"output"`
}

func (l *FileLog) Write(b []byte) (int, error) {
	if err := l.rotate(int64(len(b))); err != nil {
		return 0, l.fail(err)
	}
	n, err := l.File.Write(b)

	return n, l.fail(err)
}

// Close closes the file the log writes now, which a rotation replaced.
func (l *FileLog) Close() error {
	return l.File.Close()
}

// rotate renames a file that n more bytes would take past Max, so the bound is exact; this process is the only writer.
func (l *FileLog) rotate(n int64) error {
	info, err := l.File.Stat()
	if err != nil {
		return fmt.Errorf("measure the log: %w", err)
	}
	if info.Size() == 0 || info.Size()+n <= l.Max {
		return nil
	}

	path := l.File.Name()
	if err := os.Rename(path, logfile.Rotated(path)); err != nil {
		return fmt.Errorf("rotate the log: %w", err)
	}
	// Between the rename and the next file, so no follower takes the size or mtime change for a truncate of the live log and reads it twice.
	if err := freePastEnd(l.File, info.Size()); err != nil {
		return fmt.Errorf("free the log's space past its end: %w", err)
	}
	next, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open the log after a rotation: %w", err)
	}
	rotated := l.File
	l.File = next
	if err := rotated.Close(); err != nil {
		return fmt.Errorf("close the rotated log: %w", err)
	}

	return l.restartCursor(uint64(info.Size())) //nolint:gosec // a file size is never negative
}

// restartCursor places the new file's first byte at the output byte the rotated file ended at.
func (l *FileLog) restartCursor(size uint64) error {
	c, found, err := readCursor(l.Cursor)
	if err != nil {
		return err
	}
	// A guest that never resumed keeps no cursor, and one that no longer matches is reset by the next resume.
	if !found || size < c.Offset {
		return nil
	}

	return l.writeCursor(logCursor{Offset: 0, Output: c.Output + size - c.Offset})
}

// BoundLog bounds a log no FileLog writes now, which a daemon before the bound left past max: its last max bytes move to the rotated file, and the cursor follows the output into the emptied log.
func BoundLog(path, cursor string, max int64) error {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("measure the log: %w", err)
	}
	if info.Size() <= max {
		return nil
	}
	if err := logfile.Truncate(path, max); err != nil {
		return err
	}

	return (&FileLog{Cursor: cursor}).restartCursor(uint64(info.Size())) //nolint:gosec // a file size is never negative
}

func (l *FileLog) writeCursor(c logCursor) error {
	encoded, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal the log cursor: %w", err)
	}
	if err := store.WriteFile(l.Cursor, encoded, 0o600); err != nil {
		return fmt.Errorf("write the log cursor: %w", err)
	}

	return nil
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
	if err := l.writeCursor(logCursor{Offset: size, Output: from}); err != nil {
		return 0, l.fail(err)
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
