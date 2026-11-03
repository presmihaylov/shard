package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/pkg/sftp"
)

// runSFTP is the sftp mode main runs: 0 once the client hung up, 1 when the session broke.
func runSFTP() int {
	if err := sftpAsUser(); err != nil {
		fmt.Fprintln(os.Stderr, "shard-init:", err)

		return 1
	}

	return 0
}

// sftpAsUser sheds what a runtime handed a non-root user, as the files mode does, so the guest kernel checks that user alone.
func sftpAsUser() error {
	if os.Geteuid() != 0 {
		if err := dropCapabilities(); err != nil {
			return err
		}
	}

	return serveSFTP(stdio{in: os.Stdin, out: os.Stdout}, os.Getenv("HOME"))
}

// serveSFTP answers one sftp client until it hangs up, with relative paths from home as sshd's sftp-server has them.
func serveSFTP(conn io.ReadWriteCloser, home string) error {
	var options []sftp.ServerOption
	if home != "" {
		options = append(options, sftp.WithServerWorkingDirectory(home))
	}

	server, err := sftp.NewServer(conn, options...)
	if err != nil {
		return errors.Join(fmt.Errorf("start the sftp server: %w", err), conn.Close())
	}
	if err := server.Serve(); err != nil {
		return errors.Join(fmt.Errorf("serve sftp: %w", err), server.Close())
	}

	return server.Close()
}

// stdio is the exec's stdin and stdout as the one stream an sftp server speaks over.
type stdio struct {
	in  io.ReadCloser
	out io.WriteCloser
}

func (s stdio) Read(p []byte) (int, error) { return s.in.Read(p) }

func (s stdio) Write(p []byte) (int, error) { return s.out.Write(p) }

func (s stdio) Close() error { return errors.Join(s.in.Close(), s.out.Close()) }
