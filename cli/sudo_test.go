package cli

import "testing"

// Only root on behalf of a person who typed sudo adds it; root itself and a plain user run each hint as printed. (SHARD-727)
func TestUnderSudo(t *testing.T) {
	cases := []struct {
		name     string
		euid     int
		sudoUser string
		want     bool
	}{
		{"sudo shard", 0, "u", true},
		{"root itself", 0, "", false},
		{"a plain user", 1000, "u", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := func(k string) string {
				if k == "SUDO_USER" {
					return c.sudoUser
				}
				return ""
			}
			if got := underSudo(c.euid, env); got != c.want {
				t.Errorf("underSudo(%d, SUDO_USER=%q) = %v, want %v", c.euid, c.sudoUser, got, c.want)
			}
		})
	}
}

// Every shard command a hint names gains sudo once; prose that names shard, and a command that has sudo, stay.
func TestWithSudo(t *testing.T) {
	cases := map[string]string{
		"1 stopped sandbox; shard list --all":                               "1 stopped sandbox; sudo shard list --all",
		"start it again with shard start demo":                              "start it again with sudo shard start demo",
		"its app had not ended; shard start x runs it again":                "its app had not ended; sudo shard start x runs it again",
		`unknown command "x"; run shard help`:                               `unknown command "x"; run sudo shard help`,
		"pipe it in: echo v | shard secret set --destination h k":           "pipe it in: echo v | sudo shard secret set --destination h k",
		"--provider is a shard daemon flag: shard daemon --provider gvisor": "--provider is a shard daemon flag: sudo shard daemon --provider gvisor",

		"cannot connect to the shard daemon at /run/shard/api.sock: is it running? sudo shard daemon --provider gvisor": "cannot connect to the shard daemon at /run/shard/api.sock: is it running? sudo shard daemon --provider gvisor",
		"run: sudo shard list":                        "run: sudo shard list",
		": shard captured no output from it":          ": shard captured no output from it",
		"upgrade: shard release v0.1.2 has no file x": "upgrade: shard release v0.1.2 has no file x",
	}
	for in, want := range cases {
		if got := withSudo(in); got != want {
			t.Errorf("withSudo(%q) = %q, want %q", in, got, want)
		}
	}
}
