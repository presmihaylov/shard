package cli

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The site serves the committed pages, so a help change that skips make cli-docs fails here and not on the site.
func TestTheCommittedCLIReferenceIsTheOneTheHelpMakes(t *testing.T) {
	for _, file := range ReferenceFiles() {
		path := filepath.Join("..", ReferenceDir, file)
		committed, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		page, err := ReferencePage(file, string(committed))
		if err != nil {
			t.Fatalf("ReferencePage(%s): %v", file, err)
		}
		if page != string(committed) {
			t.Errorf("%s/%s differs from cli/help.go; run make cli-docs", ReferenceDir, file)
		}
	}
}

func TestEveryVerbHasOneReferencePage(t *testing.T) {
	var paged, grouped []string
	for _, p := range referencePages {
		paged = append(paged, p.verbs...)
	}
	for _, group := range verbGroups {
		grouped = append(grouped, group.verbs...)
	}

	slices.Sort(paged)
	slices.Sort(grouped)
	if !slices.Equal(paged, grouped) {
		t.Errorf("the reference pages hold %v, want each verb of the help once: %v", paged, grouped)
	}
}

func TestAPageKeepsItsHandWrittenPart(t *testing.T) {
	page, err := ReferencePage("snapshots.mdx", "old\n"+referenceEnd+"\n\n## By hand\n")
	if err != nil {
		t.Fatalf("ReferencePage: %v", err)
	}
	if want := referenceEnd + "\n\n## By hand\n"; page[len(page)-len(want):] != want {
		t.Errorf("the page ends %q, want the hand-written part %q", page[len(page)-len(want):], want)
	}

	if _, err := ReferencePage("snapshots.mdx", "a page without the marker"); err == nil {
		t.Error("ReferencePage replaced a page with no end marker")
	}
}

func TestProseMarksCodeAndEscapesMarkdown(t *testing.T) {
	cases := map[string]string{
		"Use 'shard exec' to execute commands.":                "Use `shard exec` to execute commands.",
		"The image's default command":                          "The image's default command",
		"Prefix paths with './' or '/'.":                       "Prefix paths with `./` or `/`.",
		"refuses --remote, SHARD_REMOTE and a saved one":       "refuses `--remote`, `SHARD_REMOTE` and a saved one",
		"Commands receive $NAME with a placeholder":            "Commands receive `$NAME` with a placeholder",
		"Shard replaces it in <root>/auth {x} *bold* [a]":      `shard replaces it in \<root\>/auth \{x\} \*bold\* \[a\]`,
		"'*.example.com' matches names under example.com":      "`*.example.com` matches names under example.com",
		"Use '--allow dns' to allow DNS. '--deny dns' is not.": "Use `--allow dns` to allow DNS. `--deny dns` is not.",
	}
	for in, want := range cases {
		if got := prose(in); got != want {
			t.Errorf("prose(%q) = %q, want %q", in, got, want)
		}
	}
}
