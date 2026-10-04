package cli

import (
	"bytes"
	"errors"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/presmihaylov/shard/models"
)

// Every stub holds its final shape: it parses its flags, then exits 3 with its verb path and prints nothing to stdout.
func TestEveryStubExitsThreeWithItsVerbAndNoStdout(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"snapshot", "create", "web"}, "snapshot create"},
		{[]string{"snapshot", "create", "--name", "base", "web"}, "snapshot create"},
		{[]string{"snapshot", "list"}, "snapshot list"},
		{[]string{"snapshot", "ls"}, "snapshot list"},
		{[]string{"snapshot", "list", "--format", "json"}, "snapshot list"},
		{[]string{"snapshot", "inspect", "base"}, "snapshot inspect"},
		{[]string{"snapshot", "inspect", "--format", "table", "base"}, "snapshot inspect"},
		{[]string{"snapshot", "remove", "base"}, "snapshot remove"},
		{[]string{"snapshot", "rm", "base"}, "snapshot remove"},
		{[]string{"create", "--snapshot", "base"}, "create --snapshot"},
		{[]string{"list", "--format", "json"}, "list --format json"},
		{[]string{"ls", "--all", "--format", "json"}, "list --format json"},
		{[]string{"inspect", "--format", "table", "web"}, "inspect --format table"},
		{[]string{"image", "list", "--format", "json"}, "image list --format json"},
		{[]string{"image", "ls", "--format", "json"}, "image list --format json"},
		{[]string{"secret", "list", "--format", "json"}, "secret list --format json"},
		{[]string{"policy", "list", "--format", "json"}, "policy list --format json"},
		{[]string{"policy", "show", "--format", "table", "web-only"}, "policy show --format table"},
		{[]string{"tokens", "list", "--format", "json"}, "tokens list --format json"},
		{[]string{"tokens", "mint", "--name", "ci", "--format", "table"}, "tokens mint --format table"},
		{[]string{"info", "--format", "json"}, "info --format json"},
		{[]string{"daemon", "status", "--format", "json"}, "daemon status --format json"},
		{[]string{"version", "--format", "json"}, "version --format json"},
	}

	for _, c := range cases {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			var out bytes.Buffer
			app := App{Version: "test", Root: t.TempDir(), Out: &out}

			err := app.Run(t.Context(), c.args)

			var exit *ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("returned %v, want an exit of %d", err, NotImplementedExitCode)
			}
			if exit.Code != NotImplementedExitCode || exit.Message != c.want+": not implemented yet" {
				t.Errorf("exit %d %q, want %d %q", exit.Code, exit.Message, NotImplementedExitCode, c.want+": not implemented yet")
			}
			if out.Len() != 0 {
				t.Errorf("stdout holds %q, want nothing", out.String())
			}
		})
	}
}

// A stub still refuses a usage it would refuse once it lands, with the exit of any other error.
func TestAStubRefusesABadUsageBeforeItExitsThree(t *testing.T) {
	for _, args := range [][]string{
		{"snapshot", "create"},
		{"snapshot", "list", "extra"},
		{"snapshot", "inspect"},
		{"snapshot", "remove"},
		{"snapshot", "create", "--name", "bad/name", "web"},
		{"snapshot", "create", "--name", "", "web"},
		{"list", "--format", "yaml"},
		{"create", "--snapshot", "base", "alpine:3.20"},
		{"tokens", "mint", "--format", "table"},
		{"tokens", "mint", "--name", "ci", "--duration", "-1h", "--format", "table"},
		{"tokens", "mint", "--name", "ci", "--scopes", "nope", "--format", "table"},
	} {
		err := newApp(t, &bytes.Buffer{}).Run(t.Context(), args)

		var exit *ExitError
		if err == nil || errors.As(err, &exit) {
			t.Errorf("shard %s returned %v, want a usage error", strings.Join(args, " "), err)
		}
	}
}

// An alias is a second spelling of its primary, so it prints the primary's help.
func TestEachAliasPrintsItsPrimarysHelp(t *testing.T) {
	for _, cmd := range commands() {
		checkAliasHelp(t, nil, cmd)
		for _, sub := range cmd.subs {
			checkAliasHelp(t, []string{cmd.name}, sub)
		}
	}
}

func checkAliasHelp(t *testing.T, parent []string, cmd command) {
	t.Helper()

	primary := append(slices.Clone(parent), cmd.name)
	for _, alias := range cmd.aliases {
		spelled := append(slices.Clone(parent), alias)
		if helpOf(t, append(spelled, "--help")...).text != helpOf(t, append(slices.Clone(primary), "--help")...).text {
			t.Errorf("shard %s --help differs from shard %s --help", strings.Join(spelled, " "), strings.Join(primary, " "))
		}
	}
}

func TestTheLongFlagsParseAsTheShortOnes(t *testing.T) {
	short, err := parseExec([]string{"-it", "web", "sh"})
	if err != nil {
		t.Fatalf("exec -it: %v", err)
	}
	for _, args := range [][]string{{"-i", "-t", "web", "sh"}, {"--interactive", "--tty", "web", "sh"}, {"--interactive", "-t", "web", "sh"}} {
		long, err := parseExec(args)
		if err != nil {
			t.Fatalf("exec %v: %v", args, err)
		}
		if !reflect.DeepEqual(long, short) || !long.interactive || !long.tty {
			t.Errorf("exec %v parsed %+v, want %+v", args, long, short)
		}
	}

	f, err := parseLogs([]string{"-f", "web"})
	if err != nil {
		t.Fatalf("logs -f: %v", err)
	}
	follow, err := parseLogs([]string{"--follow", "web"})
	if err != nil {
		t.Fatalf("logs --follow: %v", err)
	}
	if !reflect.DeepEqual(follow, f) || !follow.follow {
		t.Errorf("logs --follow parsed %+v, want %+v", follow, f)
	}
}

// The format each verb writes today is the one it names, so naming it changes nothing.
func TestTheNamedFormatIsTheOutputTheVerbAlreadyWrites(t *testing.T) {
	var out bytes.Buffer

	down := []models.Sandbox{
		{ID: "down-1", Name: "web", Image: "alpine:3.20", State: models.StateStopped, CreatedAt: time.Now(), Address: netip.MustParsePrefix("10.44.0.2/24")},
		{ID: "down-2", Image: "alpine:3.20", State: models.StateStopped, CreatedAt: time.Now(), Address: netip.MustParsePrefix("10.44.0.3/24")},
	}
	lists := newListApp(t, &out, down, nil)
	inspects, _ := newClientApp(t, &out, stopped())
	images, _ := newStoreApp(t, &out)
	secrets, _ := newSecretApp(t, &out, "", &fakeLifecycleRepo{r: &recorder{}})
	local := App{Version: "test", Root: t.TempDir(), Out: &out}

	cases := []struct {
		app   App
		plain []string
		named []string
	}{
		{lists, []string{"list", "--all"}, []string{"list", "--all", "--format", "table"}},
		{lists, []string{"list", "--all"}, []string{"ls", "--all", "--format", "table"}},
		{inspects, []string{"inspect", "web"}, []string{"inspect", "--format", "json", "web"}},
		{images, []string{"image", "list"}, []string{"image", "list", "--format", "table"}},
		{images, []string{"version"}, []string{"version", "--format", "table"}},
		{secrets, []string{"secret", "list"}, []string{"secret", "list", "--format", "table"}},
		{local, []string{"tokens", "list"}, []string{"tokens", "list", "--format", "table"}},
		{local, []string{"info"}, []string{"info", "--format", "table"}},
	}

	for _, c := range cases {
		out.Reset()
		if err := c.app.Run(t.Context(), c.plain); err != nil {
			t.Fatalf("shard %s: %v", strings.Join(c.plain, " "), err)
		}
		plain := out.String()

		out.Reset()
		if err := c.app.Run(t.Context(), c.named); err != nil {
			t.Fatalf("shard %s: %v", strings.Join(c.named, " "), err)
		}
		if out.String() != plain || plain == "" {
			t.Errorf("shard %s printed\n%s\nwant what shard %s printed\n%s", strings.Join(c.named, " "), out.String(), strings.Join(c.plain, " "), plain)
		}
	}
}
