package cli

import (
	"flag"
	"strings"
	"testing"
)

func TestParseMiBConvertsEachUnitToWholeMiB(t *testing.T) {
	cases := map[string]int64{
		"512":            512,
		"0":              0,
		"0GiB":           0,
		"0KB":            0,
		"512MiB":         512,
		"2GiB":           2048,
		"1024KiB":        1,
		"1025KiB":        2,
		"1KB":            1,
		"1MB":            1,
		"2MB":            2,
		"1GB":            954,
		"8589934591GiB":  8796093021184,
		"9223372036GB":   8796093021393,
		"0000512MiB":     512,
		"18014398509481": 18014398509481,
	}
	for value, want := range cases {
		got, err := parseMiB(value)
		if err != nil || got != want {
			t.Errorf("parseMiB(%q) = %d, %v; want %d", value, got, err, want)
		}
	}
}

func TestParseMiBRefusesWhatItCannotCountExactly(t *testing.T) {
	cases := map[string]string{
		"":                     "want a whole size",
		"MiB":                  "want a whole size",
		"+5":                   "want a whole size",
		"-1":                   "not negative",
		"-1GiB":                "not negative",
		"1.5GiB":               "a fraction is never rounded",
		"0.5":                  "a fraction is never rounded",
		"1,5GB":                "a fraction is never rounded",
		"512mb":                `unknown unit "mb"`,
		"512 MiB":              `unknown unit " MiB"`,
		"1TB":                  `unknown unit "TB"`,
		"2G":                   `unknown unit "G"`,
		"9223372036854775808":  "too large",
		"8589934592GiB":        "too large",
		"9223372037GB":         "too large",
		"99999999999999999999": "too large",
	}
	for value, want := range cases {
		got, err := parseMiB(value)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseMiB(%q) = %d, %v; want an error that says %q", value, got, err, want)
		}
	}
}

func TestSizeFlagKeepsWholeMiB(t *testing.T) {
	var memory int64
	flags := flag.NewFlagSet("shard create", flag.ContinueOnError)
	flags.Var(sizeMiB{&memory}, "memory", "")

	if err := flags.Parse([]string{"--memory", "2GiB"}); err != nil {
		t.Fatalf("parse --memory 2GiB: %v", err)
	}
	if memory != 2048 || flags.Lookup("memory").Value.String() != "2048" {
		t.Errorf("--memory 2GiB holds %d, want 2048", memory)
	}
}
