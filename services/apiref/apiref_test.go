package apiref

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/services/api"
)

// The site serves the committed pages, so a route change that skips make openapi fails here and not on the site.
func TestTheCommittedAPIReferenceIsTheOneTheRoutesMake(t *testing.T) {
	for _, file := range Files() {
		path := filepath.Join("..", "..", Dir, file)
		committed, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		page, err := Page(file, string(committed))
		if err != nil {
			t.Fatalf("Page(%s): %v", file, err)
		}
		if page != string(committed) {
			t.Errorf("%s/%s differs from the routes; run make openapi", Dir, file)
		}
	}
}

func TestEveryTagHasOnePageAndEveryRouteOneSection(t *testing.T) {
	r, err := newRenderer()
	if err != nil {
		t.Fatalf("newRenderer: %v", err)
	}

	var paged, tagged []string
	for _, p := range pages[1:] {
		paged = append(paged, p.tag)
	}
	for tag := range r.byTag {
		tagged = append(tagged, tag)
	}
	slices.Sort(paged)
	slices.Sort(tagged)
	if !slices.Equal(paged, tagged) {
		t.Errorf("the pages hold the tags %v, want each tag of the spec once: %v", paged, tagged)
	}

	public := 0
	for _, route := range api.Routes() {
		if route.Class == api.Public {
			public++
		}
	}
	sections := 0
	for _, p := range pages[1:] {
		sections += len(r.byTag[p.tag])
	}
	if sections != public {
		t.Errorf("the pages hold %d routes, want the %d public ones", sections, public)
	}
}

func TestEveryObjectLivesOnAPage(t *testing.T) {
	r, err := newRenderer()
	if err != nil {
		t.Fatalf("newRenderer: %v", err)
	}

	for name := range r.spec.Components.Schemas {
		if r.home[name] == "" {
			t.Errorf("no page uses the object %s, so the reference never shows it", name)
		}
	}
}

func TestAPageKeepsItsHandWrittenParts(t *testing.T) {
	current := "---\ntitle: old\n---\n\nBy hand above.\n\n" + start + "\n\nold\n\n" + end + "\n\n## By hand below\n"
	page, err := Page("meta.mdx", current)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if !strings.Contains(page, "\n---\n\nBy hand above.\n\n"+start) {
		t.Errorf("the page lost the part above the start marker:\n%s", page)
	}
	if !strings.HasSuffix(page, end+"\n\n## By hand below\n") {
		t.Errorf("the page lost the part below the end marker:\n%s", page)
	}
	if strings.Contains(page, "\nold\n") {
		t.Errorf("the page kept the old generated part:\n%s", page)
	}

	for _, broken := range []string{"no frontmatter " + start + end, "---\ntitle: x\n---\n" + end, "---\ntitle: x\n---\n" + start} {
		if _, err := Page("meta.mdx", broken); err == nil {
			t.Errorf("Page replaced %q, which lacks a part it keeps", broken)
		}
	}
}

func TestTypeOfNamesAndLinksASchema(t *testing.T) {
	r := &renderer{home: map[string]string{"Sandbox": "sandboxes.mdx", "Error": "index.mdx"}}
	cases := map[string]string{
		`{"$ref":"#/components/schemas/Sandbox"}`:                                           "[Sandbox](#sandbox)",
		`{"$ref":"#/components/schemas/Error"}`:                                             "[Error](/docs/reference/api/#error)",
		`{"type":"array","items":{"$ref":"#/components/schemas/Sandbox"}}`:                  "array of [Sandbox](#sandbox)",
		`{"anyOf":[{"$ref":"#/components/schemas/Sandbox"},{"type":"null"}]}`:               "[Sandbox](#sandbox) or null",
		`{"type":["string","null"],"format":"date-time"}`:                                   "string (date-time) or null",
		`{"type":"string","format":"binary","contentMediaType":"application/octet-stream"}`: "binary",
	}
	for in, want := range cases {
		var s schema
		if err := json.Unmarshal([]byte(in), &s); err != nil {
			t.Fatalf("decode %s: %v", in, err)
		}
		if got := r.typeOf(&s, "sandboxes.mdx"); got != want {
			t.Errorf("typeOf(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestAnchorIsTheIDTheSiteGives(t *testing.T) {
	cases := map[string]string{
		"Save a sandbox's state and suspend it": "save-a-sandboxs-state-and-suspend-it",
		"Read, wait for or attach to an exec":   "read-wait-for-or-attach-to-an-exec",
		"ErrorObject":                           "errorobject",
	}
	for in, want := range cases {
		if got := anchor(in); got != want {
			t.Errorf("anchor(%q) = %q, want %q", in, got, want)
		}
	}
}
