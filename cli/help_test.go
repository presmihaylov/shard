package cli

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// helpPath is one command the dispatcher runs, as the words typed after shard, with the spellings that reach it.
type helpPath struct {
	key   string
	words [][]string
	cmd   command
}

// helpPaths walks the dispatcher table, so a verb added to it is walked without a word here.
func helpPaths() []helpPath {
	var paths []helpPath
	for _, cmd := range commands() {
		paths = append(paths, helpPath{key: cmd.name, words: [][]string{{cmd.name}}, cmd: cmd})
		for _, sub := range cmd.subs {
			words := [][]string{{cmd.name, sub.name}}
			for _, alias := range sub.aliases {
				words = append(words, []string{cmd.name, alias})
			}
			paths = append(paths, helpPath{key: cmd.name + " " + sub.name, words: words, cmd: sub})
		}
	}

	return paths
}

// helpOf runs one --help through the dispatcher and answers what it would print, with no daemon under the root.
func helpOf(t *testing.T, args ...string) printExit {
	t.Helper()

	return helpUnder(t, App{Version: "test", Root: t.TempDir()}, args...)
}

// badRemote points every verb at a shard serve whose token file does not exist.
func badRemote(t *testing.T) App {
	return App{Version: "test", Root: t.TempDir(), Remote: "https://example.invalid", TokenFile: filepath.Join(t.TempDir(), "missing")}
}

func helpUnder(t *testing.T, app App, args ...string) printExit {
	t.Helper()

	err := app.run(t.Context(), args)

	var exit printExit
	if !errors.As(err, &exit) {
		t.Fatalf("shard %s returned %v, want its help", strings.Join(args, " "), err)
	}

	return exit
}

// Every word the dispatcher takes has a help, a place on the top level, and a --help and -h that print it with no daemon.
func TestEveryCommandHasItsHelp(t *testing.T) {
	top := helpOf(t, "--help").text

	for _, path := range helpPaths() {
		h, ok := helps[path.key]
		if !ok || h.summary == "" || len(h.usage) == 0 {
			t.Errorf("%s has no help, or one with no usage or summary", path.key)

			continue
		}

		verb, sub, _ := strings.Cut(path.key, " ")
		if topLine(top, verb) == "" {
			t.Errorf("the top level lists no %s", verb)
		}
		if sub != "" && topLine(helpOf(t, verb, "--help").text, sub) == "" {
			t.Errorf("shard %s --help lists no %s", verb, sub)
		}

		for _, words := range path.words {
			for _, flag := range []string{"--help", "-h"} {
				args := append(slices.Clone(words), flag)
				// The help reads no remote token, so a broken --remote setup still gets it.
				for _, got := range []string{helpOf(t, args...).text, helpUnder(t, badRemote(t), args...).text} {
					if first, _, _ := strings.Cut(got, "\n"); first != "Usage: shard "+path.key && !strings.HasPrefix(first, "Usage: shard "+path.key+" ") {
						t.Errorf("shard %s printed %q, want the help of %s", strings.Join(args, " "), got, path.key)
					}
				}
			}
		}
	}

	for key := range helps {
		if _, ok := lookup(key); !ok && key != "" {
			t.Errorf("helps holds %q, which the dispatcher does not take", key)
		}
	}
}

// The token is read when a verb calls the daemon, so a real verb under a broken --remote setup still fails on it.
func TestAVerbUnderABadRemoteFailsOnTheTokenFile(t *testing.T) {
	app := badRemote(t)

	err := app.Run(t.Context(), []string{"list"})
	if want := "read the token file " + app.TokenFile; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("list under a missing token file returned %v, want %q", err, want)
	}
	if top := helpUnder(t, badRemote(t), "--help").text; !strings.HasPrefix(top, "Usage: shard ") {
		t.Errorf("shard --help under a missing token file printed %q", top)
	}
}

// topLine is the line of the top level that lists one verb.
func topLine(top, verb string) string {
	for line := range strings.SplitSeq(top, "\n") {
		// The name column ends at two spaces, so policy does not match the policy logs row.
		if name, _, _ := strings.Cut(strings.TrimPrefix(line, "  "), "  "); strings.HasPrefix(line, "  ") && name == verb {
			return line
		}
	}

	return ""
}

// Every verb sits under exactly one heading of the top level, and every heading names verbs the dispatcher takes.
func TestEveryVerbHasOneGroup(t *testing.T) {
	var grouped []string
	for _, group := range verbGroups {
		grouped = append(grouped, group.verbs...)
	}

	for _, name := range names(commands()) {
		count := 0
		for _, verb := range grouped {
			if verb == name {
				count++
			}
		}
		if count != 1 {
			t.Errorf("%s is under %d headings of the top level, want 1", name, count)
		}
	}
	for _, name := range grouped {
		if _, ok := lookup(name); !ok {
			t.Errorf("the top level lists %s, which the dispatcher does not take", name)
		}
	}
}

// The help and the flag set are one list: every flag a verb parses is in its help, as --name, and nothing else is.
func TestEveryHelpListsTheFlagsItsVerbParses(t *testing.T) {
	for _, path := range helpPaths() {
		exit := helpOf(t, append(slices.Clone(path.words[0]), "--help")...)

		var listed []string
		for _, f := range helps[path.key].flags {
			name := flagName(f.spell)
			if !strings.HasPrefix(f.spell, dashed(name)) {
				t.Errorf("the help of %s spells %q, want it to start with %s", path.key, f.spell, dashed(name))
			}
			listed = append(listed, name)
		}

		slices.Sort(listed)
		parsed := slices.Sorted(slices.Values(exit.flags))
		if !slices.Equal(listed, parsed) {
			t.Errorf("the help of %s lists %v, and its flag set parses %v", path.key, listed, parsed)
		}
	}
}

// Every help reads on an 80-column terminal.
func TestNoHelpLinePassesEightyColumns(t *testing.T) {
	texts := map[string]string{"": helpOf(t, "--help").text}
	for _, path := range helpPaths() {
		texts[path.key] = helpOf(t, append(slices.Clone(path.words[0]), "--help")...).text
	}

	for key, text := range texts {
		for line := range strings.SplitSeq(text, "\n") {
			if n := utf8.RuneCountInString(line); n > helpWidth {
				t.Errorf("the help of %q holds a line of %d columns: %q", key, n, line)
			}
		}
	}
}

// A noun with no subcommand, or one it does not take, names the ones it does, the same way for all four.
func TestANounNamesItsSubcommands(t *testing.T) {
	cases := map[string]string{
		"image":  "list, remove or prune",
		"secret": "set, list, remove, grant or ungrant",
		"policy": "create, show, list, remove, attach, detach or logs",
		"tokens": "mint, list or revoke",
	}

	for noun, subs := range cases {
		app := App{Version: "test", Root: t.TempDir()}

		err := app.Run(t.Context(), []string{noun})
		if want := noun + " takes a subcommand: " + subs; err == nil || err.Error() != want {
			t.Errorf("shard %s returned %v, want %q", noun, err, want)
		}
		err = app.Run(t.Context(), []string{noun, "bogus"})
		if want := noun + ` subcommand "bogus"; want ` + subs; err == nil || err.Error() != "unknown "+want {
			t.Errorf("shard %s bogus returned %v, want %q", noun, err, "unknown "+want)
		}
	}
}

// A sandbox name may start with -, so a -- before it reaches the daemon as the name rather than as a flag.
func TestADoubleDashPassesANameThatLooksLikeAFlag(t *testing.T) {
	app := App{Version: "test", Root: shortRoot(t)}

	err := app.Run(t.Context(), []string{"pause", "--", "-web"})
	if err == nil || !strings.Contains(err.Error(), "cannot connect to shard daemon") {
		t.Errorf("pause -- -web returned %v, want it to reach for the daemon", err)
	}
}
