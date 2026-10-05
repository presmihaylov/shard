package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/presmihaylov/shard/pkg/term"
)

// Local is what a local setup installs: one provider, and whether a service starts it at boot.
type Local struct {
	Provider    string
	StartAtBoot bool
}

// local is §5 to §10: the provider, automatic startup, preflight, review, and apply; h is what becomes of a saved remote.
func (s *Setup) local(ctx context.Context, h handover) error {
	provider, err := s.askProvider(ctx, nil)
	if err != nil {
		return err
	}
	startAtBoot, err := s.askStartAtBoot(ctx)
	if err != nil {
		return err
	}
	l, err := s.checked(ctx, Local{Provider: provider, StartAtBoot: startAtBoot})
	if err != nil {
		return err
	}
	if err := s.review(ctx, l, h.review); err != nil {
		return err
	}
	if err := s.admin(ctx); err != nil {
		return err
	}
	steps, err := s.localSteps(ctx, l)
	if err != nil {
		return err
	}
	if err := s.apply(ctx, "Setting up Shard", steps); err != nil {
		return err
	}
	note, err := h.finish(ctx)
	if err != nil {
		return err
	}

	return s.UI.Print(localDone(s.Host, l, note)...)
}

const exitOption = "exit"

// askProvider is §5; a provider that failed preflight shows why, and Exit leaves with nothing changed.
func (s *Setup) askProvider(ctx context.Context, failed map[string][]string) (string, error) {
	choices := Providers(ctx, s.Host)
	options := make([]term.Option, 0, len(choices)+1)
	available := false
	for _, c := range choices {
		o := term.Option{Name: c.Name, Label: c.Title, Lines: c.Lines, Unavailable: c.Unavailable}
		if lines, ok := failed[c.Name]; ok {
			o.Lines, o.Unavailable = nil, lines
		}
		o.Default = c.Recommended && len(o.Unavailable) == 0
		available = available || len(o.Unavailable) == 0
		options = append(options, o)
	}
	// The menu always keeps one row to choose, since a list with none refuses to draw.
	if len(failed) > 0 || !available {
		options = append(options, term.Option{Name: exitOption, Label: "Exit"})
	}
	i, err := s.UI.Select(ctx, AskProvider, "Choose how Shard isolates your sandboxes:", options)
	if err != nil {
		return "", err
	}
	if options[i].Name == exitOption {
		return "", ErrDeclined
	}

	return options[i].Name, nil
}

// askStartAtBoot is §7; the names are what --start-at-boot takes.
func (s *Setup) askStartAtBoot(ctx context.Context) (bool, error) {
	options := []term.Option{
		{Name: "true", Label: "Yes (recommended)", Default: true, Lines: []string{
			"Set up a background service using systemd on Linux or launchd on macOS.",
			"The service starts Shard at boot and restarts it after a crash.",
		}},
		{Name: "false", Label: "No", Lines: []string{
			"Manage the Shard daemon yourself.",
			"Local sandbox commands fail when the daemon is not running.",
			"Run `shard daemon` in a terminal, or configure your own background",
			"service and automatic startup.",
		}},
	}
	i, err := s.UI.Select(ctx, AskStartAtBoot, "Start Shard automatically when this machine starts?", options)
	if err != nil {
		return false, err
	}

	return options[i].Name == "true", nil
}

// checked runs preflight until it passes; a failure another provider may not have asks for the provider again.
func (s *Setup) checked(ctx context.Context, l Local) (Local, error) {
	failed := map[string][]string{}
	for {
		f, err := s.preflight(ctx, l)
		if err != nil || f == nil {
			return l, err
		}
		if err := s.UI.Print("", "No installation changes were made."); err != nil {
			return l, err
		}
		if !f.provider {
			return l, &StoppedError{Step: f.check, Err: &Problem{Lines: f.lines}}
		}
		if err := s.UI.Print("Choose another provider or exit.", ""); err != nil {
			return l, err
		}
		failed[l.Provider] = f.lines
		if l.Provider, err = s.askProvider(ctx, failed); err != nil {
			return l, err
		}
	}
}

// review is §9: the changes setup will make, and the confirmation before any of them; removal is the saved remote's line, if any.
func (s *Setup) review(ctx context.Context, l Local, removal string) error {
	title := providerTitle(l.Provider)
	startup := "No"
	if l.StartAtBoot {
		startup = "Yes"
	}
	lines := []string{"", "Ready to set up Shard", "", "Provider:          " + title, "Automatic startup: " + startup, "", "Setup will:"}
	if names := missingTools(s.Host, l.Provider).names(); len(names) > 0 {
		lines = append(lines, fmt.Sprintf("  Install the tools required by %s: %s.", title, strings.Join(names, ", ")))
	}
	if binaries := shardBinaries(s.Host); len(binaries) > 0 {
		lines = append(lines, "  Install "+strings.Join(binaries, " and ")+" in "+binDir+".")
	}
	_, err := os.Lstat(rooted(s.Host, DataDir))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		lines = append(lines, "  Create Shard's data directory.")
	case err != nil:
		return fmt.Errorf("check %s: %w", DataDir, err)
	}
	lines = append(lines, startupLine(s.Host, l))
	if removal != "" {
		lines = append(lines, removal)
	}
	lines = append(lines, "", "Administrator access is required.", "")
	if err := s.UI.Print(lines...); err != nil {
		return err
	}

	return s.confirm(ctx, true)
}

// shardBinaries are what installShard puts in binDir: shard unless it already runs from there, and shard-init on Linux.
func shardBinaries(h Host) []string {
	var names []string
	if h.Executable != shardBinary {
		names = append(names, "shard")
	}
	if h.OS == "linux" {
		names = append(names, "shard-init")
	}

	return names
}

func startupLine(h Host, l Local) string {
	switch {
	case !l.StartAtBoot:
		return "  Leave daemon startup under your control."
	case h.OS == "darwin":
		return "  Configure and start a launchd service."
	}

	return "  Configure and start a systemd service."
}

// localDone closes a local setup with what to run next, after note, which says what became of a saved remote; on Linux the API socket belongs to root, so local commands need sudo.
func localDone(h Host, l Local, note []string) []string {
	sudo := ""
	if h.OS == "linux" {
		sudo = "sudo "
	}
	lines := []string{"", "Shard is set up, and the daemon is running.", ""}
	if !l.StartAtBoot {
		lines = []string{"", "Shard is set up.", ""}
	}
	if sudo != "" {
		lines = append(lines, "Local commands run with sudo, because the API socket belongs to root.", "")
	}
	if len(note) > 0 {
		lines = append(append(lines, note...), "")
	}
	lines = append(lines, "Next steps:", "")
	if !l.StartAtBoot {
		lines = append(lines, "  Start the daemon, and run the next steps in another terminal:", "    "+sudo+"shard daemon --provider "+l.Provider, "")
	}

	return append(lines, nextSteps(sudo)...)
}
