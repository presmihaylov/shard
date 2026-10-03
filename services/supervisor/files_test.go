package supervisor

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
)

// cannedConn is a guest that already said everything: the host's header lands in sent, and reads take the canned reply.
type cannedConn struct {
	io.Reader
	sent bytes.Buffer
}

func (c *cannedConn) Write(p []byte) (int, error) { return c.sent.Write(p) }

func canned(t *testing.T, messages ...any) *cannedConn {
	t.Helper()
	var answer bytes.Buffer
	for _, m := range messages {
		if err := WriteMessage(&answer, m); err != nil {
			t.Fatal(err)
		}
	}

	return &cannedConn{Reader: &answer}
}

func TestListReadsTheCountOfEntriesTheReplyNames(t *testing.T) {
	conn := canned(t, FileReply{Stat: &models.FileStat{Type: models.FileDir}, Count: 2}, models.FileEntry{Name: "a"}, models.FileEntry{Name: "b"})

	entries, err := List(conn, "/srv")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var names []string
	for {
		entry, err := entries.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		names = append(names, entry.Name)
	}
	if strings.Join(names, ",") != "a,b" {
		t.Fatalf("the entries are %v, want a and b", names)
	}
	if !strings.Contains(conn.sent.String(), `"op":"ls"`) {
		t.Fatalf("the host sent %q, want an ls header", conn.sent.String())
	}
}

// A guest that stops short of the count it promised is a cut listing, never a shorter directory.
func TestListOfAGuestThatStoppedShortIsAnUnexpectedEOF(t *testing.T) {
	conn := canned(t, FileReply{Stat: &models.FileStat{Type: models.FileDir}, Count: 3}, models.FileEntry{Name: "a"})

	entries, err := List(conn, "/srv")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, err := entries.Next(); err != nil {
		t.Fatalf("the first entry: %v", err)
	}
	if _, err := entries.Next(); !errors.Is(err, io.ErrUnexpectedEOF) || !strings.Contains(err.Error(), "2 entries short") {
		t.Fatalf("the second entry gave %v, want io.ErrUnexpectedEOF two entries short", err)
	}
}

// The guest is not trusted, so a line with no end must stop at the bound and not at the daemon's memory.
func TestAReplyLineIsBounded(t *testing.T) {
	conn := &cannedConn{Reader: io.MultiReader(strings.NewReader(`{"error":"`), strings.NewReader(strings.Repeat("x", MaxPayload+1)))}

	if _, err := Stat(conn, "/srv"); err == nil || !strings.Contains(err.Error(), "runs past") {
		t.Fatalf("an endless reply gave %v, want the bound", err)
	}
}

func TestMkdirAndDeleteSendTheirHeaders(t *testing.T) {
	stat := &models.FileStat{Type: models.FileDir}

	mkdir := canned(t, FileReply{Stat: stat})
	if err := Mkdir(mkdir, FileHeader{Path: "/srv/a", Mode: 0o700, Parents: true}); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var header FileHeader
	if err := ReadHeader(&mkdir.sent, &header); err != nil || header != (FileHeader{Op: OpMkdir, Path: "/srv/a", Mode: 0o700, Parents: true}) {
		t.Fatalf("the mkdir sent %+v, %v", header, err)
	}

	del := canned(t, FileReply{Error: "/srv/a is a directory that is not empty", Code: FileInvalid})
	err := Delete(del, "/srv/a", false)
	var refusal *FileError
	if !errors.As(err, &refusal) || refusal.Code != FileInvalid || refusal.Op != OpDelete {
		t.Fatalf("the delete gave %v, want the guest's invalid refusal", err)
	}
	var deleted FileHeader
	if err := ReadHeader(&del.sent, &deleted); err != nil || deleted != (FileHeader{Op: OpDelete, Path: "/srv/a"}) {
		t.Fatalf("the delete sent %+v, %v", deleted, err)
	}
}
