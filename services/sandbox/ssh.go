package sandbox

import (
	"context"
	"net"

	"github.com/presmihaylov/shard/models"
)

// ShellScript picks the shell in the sandbox, and no login shell, whose /etc/profile resets the image's PATH.
const ShellScript = "command -v bash >/dev/null 2>&1 && exec bash -i; exec sh -i"

// ReadyForSSH answers the id of a sandbox a command can run in, so an ssh upgrade is refused before its 101.
func (s *Service) ReadyForSSH(ctx context.Context, ref string) (string, error) {
	id, _, err := s.readyForExec(ctx, ref)

	return id, err
}

// DialPort opens one connection to a port on the sandbox's loopback, the way a host port forward reaches it.
func (s *Service) DialPort(ctx context.Context, ref string, port uint16) (net.Conn, error) {
	if err := requireVerb(s.cfg.Provider, models.VerbPort); err != nil {
		return nil, err
	}
	id, _, err := s.readyForExec(ctx, ref)
	if err != nil {
		return nil, err
	}

	return s.cfg.Provider.DialPort(ctx, id, port)
}
