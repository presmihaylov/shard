package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/bundle"
	"github.com/presmihaylov/shard/services/sandboxstate"
	"github.com/presmihaylov/shard/services/supervisor"
)

// linkHosts is a line per link of the sandbox named self; a target whose name holds _ also answers to it spelled with -, which every resolver takes.
func linkHosts(self string, links []models.Link) []models.Host {
	claims := map[string]int{self: 1}
	for _, l := range links {
		claims[l.Target]++
		if alias := dnsAlias(l.Target); alias != l.Target {
			claims[alias]++
		}
	}

	hosts := make([]models.Host, 0, len(links))
	for _, l := range links {
		names := []string{l.Target}
		// An alias another name holds would answer two addresses, so neither target takes it.
		if alias := dnsAlias(l.Target); alias != l.Target && validHostname(alias) && claims[alias] == 1 {
			names = append(names, alias)
		}
		hosts = append(hosts, models.Host{Address: l.Address, Names: names})
	}

	return hosts
}

func dnsAlias(name string) string { return strings.ReplaceAll(name, "_", "-") }

// validHostname is one RFC 1123 label, in the lower case a sandbox name already is.
func validHostname(name string) bool {
	if name == "" || len(name) > 63 || strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return false
	}

	return !strings.ContainsFunc(name, func(c rune) bool { return (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' })
}

// hostsReported is hostsChanged for a verb whose own work stands without the lines: a guest that refuses them is reported, and the next link change or start writes them again.
func (s *Service) hostsReported(ctx context.Context, ids ...string) {
	if err := s.hostsChanged(ctx, ids...); err != nil {
		s.report(err.Error())
	}
}

// hostsChanged writes the links of each sandbox into its guest's /etc/hosts, apart, so one guest that refuses leaves the rest written.
func (s *Service) hostsChanged(ctx context.Context, ids ...string) error {
	var errs []error
	for _, id := range ids {
		if err := s.writeHosts(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("the links of sandbox %s did not reach its /etc/hosts: %w", id, err))
		}
	}

	return errors.Join(errs...)
}

// writeHosts rewrites shard's block of a running guest's /etc/hosts from its record; a guest that is not up gets it as it starts or resumes.
func (s *Service) writeHosts(ctx context.Context, id string) error {
	// A lock apart from the verb's, since a remove writes the sandboxes that linked it without holding theirs.
	unlock, err := s.lock(ctx, "hosts/"+id)
	if err != nil {
		return err
	}
	defer unlock()

	// Read under the lock, so of two writes the later one writes the newer links.
	sb, err := s.cfg.Repo.Get(id)
	if errors.Is(err, sandboxstate.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if sb.State != models.StateRunning {
		return nil
	}
	if _, _, err := s.readyForExec(ctx, id); err != nil {
		return err
	}

	// The file is root's whoever the sandbox runs as, and an exec with no user runs as the sandbox's.
	asRoot := func(ctx context.Context, spec models.ExecSpec) (models.ExitStatus, error) {
		spec.User = "0"

		return s.cfg.Provider.Exec(ctx, id, spec)
	}
	current, _, err := supervisor.GuestFiles(ctx, asRoot, bundle.MaxGuestFile)("etc/hosts")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	hosts := supervisor.WithHosts(current, linkHosts(nameOf(id, sb), sb.Links))
	if bytes.Equal(hosts, current) {
		return nil
	}

	conn, err := supervisor.OpenFiles(ctx, asRoot, "")
	if err != nil {
		return err
	}
	err = supervisor.Put(conn, supervisor.FileHeader{Path: "/etc/hosts", Size: int64(len(hosts)), Mode: DefaultFileMode}, bytes.NewReader(hosts))

	return errors.Join(err, conn.Close())
}
