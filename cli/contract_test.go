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

// A bad usage is a plain error, so it exits 1 and never an exit code of its own.
func TestABadUsageExitsOne(t *testing.T) {
	for _, args := range [][]string{
		{"snapshot", "inspect", "--format", "table"},
		{"snapshot", "create", "--name", "bad/name", "web"},
		{"snapshot", "create", "--name", "", "web"},
		{"list", "--format", "yaml"},
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

	f, err := parseLogs("logs", []string{"-f", "web"})
	if err != nil {
		t.Fatalf("logs -f: %v", err)
	}
	follow, err := parseLogs("logs", []string{"--follow", "web"})
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
