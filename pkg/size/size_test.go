package size

import (
	"strings"
	"testing"
)

func TestParseMiBConvertsEachUnitToWholeMiB(t *testing.T) {
	cases := map[string]int64{
		"0":                 0,
		"0000":              0,
		"0GiB":              0,
		"0KB":               0,
		"512MiB":            512,
		"2GiB":              2048,
		"1024KiB":           1,
		"1025KiB":           2,
		"1KB":               1,
		"1MB":               1,
		"2MB":               2,
		"1GB":               954,
		"8589934591GiB":     8796093021184,
		"9223372036GB":      8796093021393,
		"0000512MiB":        512,
		"18014398509481MiB": 18014398509481,
	}
	for value, want := range cases {
		got, err := ParseMiB(value)
		if err != nil || got != want {
			t.Errorf("ParseMiB(%q) = %d, %v; want %d", value, got, err, want)
		}
	}
}

func TestParseMiBRefusesWhatItCannotCountExactly(t *testing.T) {
	cases := map[string]string{
		"":                        "want a whole size",
		"512":                     "want a unit, such as 512MiB",
		"64":                      "want a unit, such as 64MiB",
		"0064":                    "want a unit, such as 64MiB",
		"9223372036854775808":     "want a unit",
		"MiB":                     "want a whole size",
		"+5":                      "want a whole size",
		"-1":                      "not negative",
		"-1GiB":                   "not negative",
		"1.5GiB":                  "a fraction is never rounded",
		"0.5":                     "a fraction is never rounded",
		"1,5GB":                   "a fraction is never rounded",
		"512mb":                   `unknown unit "mb"`,
		"512 MiB":                 `unknown unit " MiB"`,
		"1TB":                     `unknown unit "TB"`,
		"2G":                      `unknown unit "G"`,
		"9223372036854775808MiB":  "too large",
		"8589934592GiB":           "too large",
		"9223372037GB":            "too large",
		"99999999999999999999MiB": "too large",
	}
	for value, want := range cases {
		got, err := ParseMiB(value)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseMiB(%q) = %d, %v; want an error that says %q", value, got, err, want)
		}
	}
}

func TestFormatReadsBackExactly(t *testing.T) {
	cases := map[int64]string{0: "0MiB", 512: "512MiB", 1024: "1GiB", 47104: "46GiB", 10241: "10241MiB"}
	for mib, want := range cases {
		got := Format(mib)
		if got != want {
			t.Errorf("Format(%d) = %q, want %q", mib, got, want)
		}
		back, err := ParseMiB(got)
		if err != nil || back != mib {
			t.Errorf("ParseMiB(Format(%d)) = %d, %v", mib, back, err)
		}
	}
}

func TestShowPutsASpaceBeforeTheUnit(t *testing.T) {
	cases := map[int64]string{512: "512 MiB", 10240: "10 GiB", 10241: "10241 MiB"}
	for mib, want := range cases {
		if got := Show(mib); got != want {
			t.Errorf("Show(%d) = %q, want %q", mib, got, want)
		}
	}
}
