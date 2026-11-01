package sshd

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"

	"github.com/presmihaylov/shard/pkg/store"
)

// HostKeyFile is the daemon's one ssh host key under its root, which every sandbox presents.
const HostKeyFile = "ssh_host_ed25519_key"

// LoadHostKey reads the host key under root, and makes it on the first start, so a client pins it once per daemon.
func LoadHostKey(root string) (ssh.Signer, error) {
	path := filepath.Join(root, HostKeyFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return createHostKey(path)
	}
	if err != nil {
		return nil, fmt.Errorf("read the ssh host key: %w", err)
	}

	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("parse the ssh host key %s: %w; remove it and the daemon makes another, which every client pins again", path, err)
	}

	return signer, nil
}

func createHostKey(path string) (ssh.Signer, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate the ssh host key: %w", err)
	}

	block, err := ssh.MarshalPrivateKey(key, "shard")
	if err != nil {
		return nil, fmt.Errorf("encode the ssh host key: %w", err)
	}
	if err := store.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, fmt.Errorf("write the ssh host key: %w", err)
	}

	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return nil, fmt.Errorf("load the ssh host key: %w", err)
	}

	return signer, nil
}
