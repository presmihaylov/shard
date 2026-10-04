package supervisor

import (
	"bufio"
	"errors"
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
	OpStat   = "stat"
	OpPut    = "put"
	OpGet    = "get"
	OpList   = "ls"
	OpMkdir  = "mkdir"
	OpDelete = "delete"
	OpPack   = "pack"
	OpUnpack = "unpack"
)

// The codes a refusal carries, so the host answers 404 or 400 for what the guest refused and 500 for the rest.
const (
	FileNotFound = "not_found"
	FileInvalid  = "invalid"
)

// FileHeader opens a files operation. Size rides a put, whose bytes follow the header line; Mode and Parents ride a put and a mkdir.
type FileHeader struct {
	Op        string `json:"op"`
	Path      string `json:"path"`
	Size      int64  `json:"size,omitempty"`
	Mode      uint32 `json:"mode,omitempty"`
	Parents   bool   `json:"parents,omitempty"`
	Recursive bool   `json:"recursive,omitempty"`
}

// FileReply answers the header with the path's stat, or with why not. A get's bytes follow the line to the end of the stream, and an ls's Count entry lines.
type FileReply struct {
	Stat  *models.FileStat `json:"stat,omitempty"`
	Count int              `json:"count,omitempty"`
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

func (e *FileError) Public() string { return e.Error() }

// Stat asks the guest for the shape of one path. It never follows a final symlink.
func Stat(conn io.ReadWriter, path string) (models.FileStat, error) {
	r, err := open(conn, FileHeader{Op: OpStat, Path: path})
	if err != nil {
		return models.FileStat{}, err
	}

	reply, err := readReply(r, OpStat, path)
	if err != nil {
		return models.FileStat{}, err
	}

	return *reply.Stat, nil
}

// List answers the entries of one guest directory, sorted by name, one line at a time; it follows a final symlink, as ls does.
func List(conn io.ReadWriter, path string) (*Entries, error) {
	r, err := open(conn, FileHeader{Op: OpList, Path: path})
	if err != nil {
		return nil, err
	}

	reply, err := readReply(r, OpList, path)
	if err != nil {
		return nil, err
	}

	return &Entries{r: r, path: path, left: reply.Count}, nil
}

// Entries reads an ls's entries one line at a time, so no listing has to fit in the daemon's memory at once.
type Entries struct {
	r    *bufio.Reader
	path string
	left int
}

// Next answers the next entry, io.EOF after the last, and io.ErrUnexpectedEOF when the guest stopped short of its count.
func (e *Entries) Next() (models.FileEntry, error) {
	if e.left == 0 {
		return models.FileEntry{}, io.EOF
	}

	var entry models.FileEntry
	err := readLine(e.r, &entry)
	if errors.Is(err, io.EOF) {
		return models.FileEntry{}, fmt.Errorf("%s %s: %d entries short: %w", OpList, e.path, e.left, io.ErrUnexpectedEOF)
	}
	if err != nil {
		return models.FileEntry{}, fmt.Errorf("%s %s: read an entry: %w", OpList, e.path, err)
	}
	e.left--

	return entry, nil
}

// Mkdir makes one guest directory at header.Mode; with Parents it makes what leads to it and takes a directory already there.
func Mkdir(conn io.ReadWriter, header FileHeader) error {
	header.Op = OpMkdir
	r, err := open(conn, header)
	if err != nil {
		return err
	}

	_, err = readReply(r, OpMkdir, header.Path)

	return err
}

// Delete removes one guest path, a final symlink and never its target; a directory with anything in it needs recursive.
func Delete(conn io.ReadWriter, path string, recursive bool) error {
	r, err := open(conn, FileHeader{Op: OpDelete, Path: path, Recursive: recursive})
	if err != nil {
		return err
	}

	_, err = readReply(r, OpDelete, path)

	return err
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

// FilesConn is one files exec's stdio; CloseWrite ends the guest's stdin, which is how an unpack knows the archive is all in.
type FilesConn interface {
	io.ReadWriteCloser
	CloseWrite() error
}

// GetArchive answers the stat of one guest path and a tar of it, whose top entry is the path's base name; the tar runs to the end of the exec.
func GetArchive(conn io.ReadWriter, path string) (models.FileStat, io.Reader, error) {
	r, err := open(conn, FileHeader{Op: OpPack, Path: path})
	if err != nil {
		return models.FileStat{}, nil, err
	}

	reply, err := readReply(r, OpPack, path)
	if err != nil {
		return models.FileStat{}, nil, err
	}

	return *reply.Stat, r, nil
}

// PutArchive unpacks the tar src under the guest directory path; the guest answers once its stdin ends and everything is on disk.
func PutArchive(conn FilesConn, path string, src io.Reader) error {
	r, err := open(conn, FileHeader{Op: OpUnpack, Path: path})
	if err != nil {
		return err
	}

	noted := &notedReader{Reader: src}
	_, err = io.Copy(conn, noted)
	if noted.err != nil && !errors.Is(noted.err, io.EOF) {
		return fmt.Errorf("unpack into %s: read the archive: %w", path, noted.err)
	}
	if err != nil {
		// A guest that refused an entry stopped reading, and its reason beats the broken pipe.
		if _, replyErr := readReply(r, OpUnpack, path); replyErr != nil {
			return replyErr
		}

		return fmt.Errorf("unpack into %s: send the archive: %w", path, err)
	}
	if err := conn.CloseWrite(); err != nil {
		return fmt.Errorf("unpack into %s: end the archive: %w", path, err)
	}
	if _, err := readReply(r, OpUnpack, path); err != nil {
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

	reply, err := readReply(r, OpGet, path)
	if err != nil {
		return models.FileStat{}, nil, err
	}

	return *reply.Stat, r, nil
}

// open sends the header and answers a reader of what the guest says back.
func open(conn io.ReadWriter, header FileHeader) (*bufio.Reader, error) {
	if err := WriteMessage(conn, header); err != nil {
		return nil, fmt.Errorf("%s %s: %w", header.Op, header.Path, err)
	}

	return bufio.NewReader(conn), nil
}

// readReply takes the guest's answer, which always carries a stat; a refusal comes back as a FileError with the guest's code.
func readReply(r *bufio.Reader, op, path string) (FileReply, error) {
	var reply FileReply
	if err := readLine(r, &reply); err != nil {
		return FileReply{}, fmt.Errorf("%s %s: read the guest's reply: %w", op, path, err)
	}
	if reply.Error != "" {
		return FileReply{}, &FileError{Op: op, Path: path, Code: reply.Code, Message: reply.Error}
	}
	if reply.Stat == nil {
		return FileReply{}, fmt.Errorf("%s %s: the guest answered with no stat", op, path)
	}

	return reply, nil
}

// readLine takes one JSON line of at most MaxPayload bytes, so a guest cannot grow the daemon's memory without bound.
func readLine(r *bufio.Reader, value any) error {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > MaxPayload {
			return fmt.Errorf("a line runs past %d bytes", MaxPayload)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(line) == 0 {
			return io.EOF
		}
		if err != nil {
			return fmt.Errorf("read a line: %w", err)
		}

		return DecodeFrame(line, value)
	}
}
