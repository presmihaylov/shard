package cli

import (
	"os"
	"regexp"
	"strings"
)

// ForUser words the commands text names for the user who ran this process, so each works as printed.
func ForUser(text string) string {
	if !underSudo(os.Geteuid(), os.Getenv) {
		return text
	}

	return withSudo(text)
}

// underSudo says root runs this process for a user who started it with sudo; root itself runs a command bare.
func underSudo(euid int, env func(string) string) bool {
	return euid == 0 && env("SUDO_USER") != ""
}

// commandHint is a shard verb after the words a hint starts a command with, so prose such as "a shard daemon" stays as it is.
func commandHint() *regexp.Regexp {
	verbs := append(names(commands()), "help")

	return regexp.MustCompile(`(?:\b(?:with|[Rr]un)|[:;|]) (?:sudo )?shard (?:` + strings.Join(verbs, "|") + `)\b`)
}

// withSudo puts sudo before each shard command text names that has none.
func withSudo(text string) string {
	return commandHint().ReplaceAllStringFunc(text, func(hint string) string {
		if strings.Contains(hint, "sudo ") {
			return hint
		}

		return strings.Replace(hint, "shard ", "sudo shard ", 1)
	})
}
