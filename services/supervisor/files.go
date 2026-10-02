package supervisor

import (
	"bufio"
	"fmt"
	"io"

	"github.com/presmihaylov/shard/models"
)

// InitPath is where a container sandbox mounts shard-init; a VM's shard-init answers the same path with itself.
const InitPath = "/.shard/init"

// FilesMode is shard-init's one argument for a files operation, which it serves over its stdin and stdout.
const FilesMode = "files"

// The operations a files exec carries, one per exec: the header names it, the reply ends it.
const (
	OpStat = "stat"
	OpPut  = "put"
	OpGet  = "get"
)

// The codes a refusal carries, so the host answers 404 or 400 for what the guest refused and 500 for the rest.
const (
	FileNotFound = "not_found"
	FileInvalid  = "invalid"
)

// FileHeader opens a files operation. Size, Mode and Parents ride a put; the bytes follow the header line.
type FileHeader struct {
	Op      string `json:"op"`
	Path    string `json:"path"`
	Size    int64  `json:"size,omitempty"`
	Mode    uint32 `json:"mode,omitempty"`
	Parents bool   `json:"parents,omitempty"`
}

// FileReply answers the header with the path's stat, or with why not. On a get the file's bytes follow the line, to the end of the stream.
type FileReply struct {
	Stat  *models.FileStat `json:"stat,omitempty"`
	Error string           `json:"error,omitempty"`
	Code  string           `json:"code,omitempty"`
}

// FileError is the guest's refusal of one operation, in the guest's own words.
type FileError struct {
	Op      string
	Path    string
	Code    string
	Message string
}

func (e *FileError) Error() string {
	return fmt.Sprintf("%s %s: %s", e.Op, e.Path, e.Message)
}

// Stat asks the guest for the shape of one path. It never follows a final symlink.
func Stat(conn io.ReadWriter, path string) (models.FileStat, error) {
	r, err := open(conn, FileHeader{Op: OpStat, Path: path})
	if err != nil {
		return models.FileStat{}, err
	}

	return readReply(r, OpStat, path)
}

// Put lands header.Size bytes of src at header.Path as one file; the guest answers only once the whole file is in place.
func Put(conn io.ReadWriter, header FileHeader, src io.Reader) error {
	header.Op = OpPut
	r, err := open(conn, header)
	if err != nil {
		return err
	}

	noted := &notedReader{Reader: src}
	n, err := io.CopyN(conn, noted, header.Size)
	// A reader may hand over its last bytes with io.EOF, as an http body does, so only a short copy is the source's fault.
	if n < header.Size && noted.err != nil {
		// The source ran out or failed; the caller's close is what lets the guest drop its half-copied temp name.
		return fmt.Errorf("put %s: read the source after %d of %d bytes: %w", header.Path, n, header.Size, noted.err)
	}
	if err != nil {
		// A guest that refused the header stopped reading, and its reason beats the broken pipe.
		if _, replyErr := readReply(r, OpPut, header.Path); replyErr != nil {
			return replyErr
		}

		return fmt.Errorf("put %s: send %d bytes: %w", header.Path, header.Size, err)
	}
	if _, err := readReply(r, OpPut, header.Path); err != nil {
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

// Get answers one guest file's stat and a reader of its bytes to the end of the stream, since a /proc file's Size is not its length; conn tells a cut end from a whole one.
func Get(conn io.ReadWriter, path string) (models.FileStat, io.Reader, error) {
	r, err := open(conn, FileHeader{Op: OpGet, Path: path})
	if err != nil {
		return models.FileStat{}, nil, err
	}

	stat, err := readReply(r, OpGet, path)
	if err != nil {
		return models.FileStat{}, nil, err
	}

	return stat, r, nil
}

// open sends the header and answers a reader of what the guest says back.
func open(conn io.ReadWriter, header FileHeader) (*bufio.Reader, error) {
	if err := WriteMessage(conn, header); err != nil {
		return nil, fmt.Errorf("%s %s: %w", header.Op, header.Path, err)
	}

	return bufio.NewReader(conn), nil
}

// readReply takes the guest's answer; a refusal comes back as a FileError with the guest's code.
func readReply(r *bufio.Reader, op, path string) (models.FileStat, error) {
	var reply FileReply
	if err := ReadMessage(r, &reply); err != nil {
		return models.FileStat{}, fmt.Errorf("%s %s: read the guest's reply: %w", op, path, err)
	}
	if reply.Error != "" {
		return models.FileStat{}, &FileError{Op: op, Path: path, Code: reply.Code, Message: reply.Error}
	}
	if reply.Stat == nil {
		return models.FileStat{}, fmt.Errorf("%s %s: the guest answered with no stat", op, path)
	}

	return *reply.Stat, nil
}
