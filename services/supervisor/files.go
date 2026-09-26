package supervisor

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"time"
)

// The operations a files connection carries, one per connection: the header names it, the reply ends it.
const (
	OpStat = "stat"
	OpPut  = "put"
	OpGet  = "get"
)

// FileHeader opens a files connection. Size and Mode ride a put; the bytes follow the header line.
type FileHeader struct {
	Op   string `json:"op"`
	Path string `json:"path"`
	Size int64  `json:"size,omitempty"`
	Mode uint32 `json:"mode,omitempty"`
}

// FileReply answers the header. Stat is the file as the guest sees it; on a get its Size bytes follow the line.
type FileReply struct {
	Stat  *FileStat `json:"stat,omitempty"`
	Error string    `json:"error,omitempty"`
}

// FileStat is the shape of one guest path, in the fs.FileMode bits the host reads directly.
type FileStat struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Mode    uint32    `json:"mode"`
	ModTime time.Time `json:"modtime"`
	Dir     bool      `json:"dir,omitempty"`
}

// Stat asks the guest for the shape of one path.
func Stat(ctx context.Context, dial Dialer, path string) (FileStat, error) {
	conn, r, err := openFiles(ctx, dial, FileHeader{Op: OpStat, Path: path})
	if err != nil {
		return FileStat{}, err
	}
	defer conn.Close()

	reply, err := readReply(r, OpStat, path)
	if err != nil {
		return FileStat{}, err
	}

	return *reply.Stat, nil
}

// Put lands size bytes of src at path in the guest, as one file with mode; the guest reports only once the whole file is in place.
func Put(ctx context.Context, dial Dialer, path string, mode fs.FileMode, size int64, src io.Reader) error {
	conn, r, err := openFiles(ctx, dial, FileHeader{Op: OpPut, Path: path, Size: size, Mode: uint32(mode)})
	if err != nil {
		return err
	}
	defer conn.Close()

	noted := &notedReader{Reader: src}
	n, err := io.CopyN(conn, noted, size)
	if noted.err != nil {
		// The source ran out or failed; the close below is what lets the guest drop its half-copied temp name.
		return fmt.Errorf("put %s: read the source after %d of %d bytes: %w", path, n, size, noted.err)
	}
	if err != nil {
		// A guest that refused the header hung up mid-copy, and its reason beats the broken pipe.
		if _, replyErr := readReply(r, OpPut, path); replyErr != nil {
			return replyErr
		}

		return fmt.Errorf("put %s: send %d bytes: %w", path, size, err)
	}
	if _, err := readReply(r, OpPut, path); err != nil {
		return err
	}

	return nil
}

// notedReader keeps the source's own error apart from the connection's, so a put names which side failed.
type notedReader struct {
	io.Reader
	err error
}

func (n *notedReader) Read(p []byte) (int, error) {
	count, err := n.Reader.Read(p)
	if err != nil {
		n.err = err
	}

	return count, err
}

// Get copies one guest file into dst and returns its shape; a short stream is an error, never a short file.
func Get(ctx context.Context, dial Dialer, path string, dst io.Writer) (FileStat, error) {
	conn, r, err := openFiles(ctx, dial, FileHeader{Op: OpGet, Path: path})
	if err != nil {
		return FileStat{}, err
	}
	defer conn.Close()

	reply, err := readReply(r, OpGet, path)
	if err != nil {
		return FileStat{}, err
	}
	if _, err := io.CopyN(dst, r, reply.Stat.Size); err != nil {
		return FileStat{}, fmt.Errorf("get %s: receive %d bytes: %w", path, reply.Stat.Size, err)
	}

	return *reply.Stat, nil
}

// openFiles dials the files port and sends the header; the connection closes with ctx, which is what ends a stalled copy.
func openFiles(ctx context.Context, dial Dialer, header FileHeader) (net.Conn, *bufio.Reader, error) {
	conn, err := dial(ctx, FilesPort)
	if err != nil {
		return nil, nil, fmt.Errorf("open a files connection: %w", err)
	}
	context.AfterFunc(ctx, func() { _ = conn.Close() })
	if err := WriteMessage(conn, header); err != nil {
		_ = conn.Close()

		return nil, nil, fmt.Errorf("%s %s: %w", header.Op, header.Path, err)
	}

	return conn, bufio.NewReader(conn), nil
}

// readReply takes the guest's answer; its error text is the guest's own, behind the operation and the path.
func readReply(r *bufio.Reader, op, path string) (FileReply, error) {
	var reply FileReply
	if err := ReadMessage(r, &reply); err != nil {
		return FileReply{}, fmt.Errorf("%s %s: %w", op, path, err)
	}
	if reply.Error != "" {
		return FileReply{}, fmt.Errorf("%s %s: %s", op, path, reply.Error)
	}
	if reply.Stat == nil {
		return FileReply{}, fmt.Errorf("%s %s: the guest answered with no stat", op, path)
	}

	return reply, nil
}
