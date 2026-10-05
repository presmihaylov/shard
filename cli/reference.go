package cli

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// ReferenceDir is where make cli-docs writes the CLI reference, relative to the repository root.
const ReferenceDir = "website/src/content/docs/docs/reference/cli"

// referenceEnd closes what make cli-docs writes; a page keeps the hand-written text below it.
const referenceEnd = "{/* make cli-docs writes everything above this line. Write by hand below it. */}"

const referenceStart = "{/* make cli-docs writes this part from cli/help.go. Edit the help, not this part. */}"

// referencePage is one page of the CLI reference: a group of verbs, or the overview when verbs is nil.
type referencePage struct {
	file, title, description string
	verbs                    []string
}

// referencePages are the sidebar entries in order, so a page's position is its sidebar order.
var referencePages = []referencePage{
	{"index.mdx", "Overview", "The shard command line, with its global options, environment variables, commands and JSON output.", nil},
	{"sandboxes.mdx", "Sandboxes", "The shard commands that create, run, inspect and manage sandboxes.", verbGroups[0].verbs},
	{"images.mdx", "Images", "The shard commands that download, list and delete images.", []string{"pull", "image"}},
	{"snapshots.mdx", "Snapshots", "The shard commands that save a stopped sandbox's files as a snapshot and manage snapshots.", []string{"snapshot"}},
	{"secrets.mdx", "Secrets", "The shard commands that store secrets and choose which sandboxes may use them.", []string{"secret"}},
	{"policies.mdx", "Network policies", "The shard commands that store network policies, attach them to sandboxes and show their decisions.", []string{"policy"}},
	{"host.mdx", "Host and access", "The shard commands that set up a host, run the daemon and the API server, and manage API tokens.", verbGroups[2].verbs},
}

// ReferenceFiles names every page make cli-docs writes, in sidebar order.
func ReferenceFiles() []string {
	files := make([]string, 0, len(referencePages))
	for _, p := range referencePages {
		files = append(files, p.file)
	}

	return files
}

// ReferencePage renders one page from the help and keeps what current holds below the end marker.
func ReferencePage(file, current string) (string, error) {
	tail := "\n"
	if current != "" {
		_, after, found := strings.Cut(current, referenceEnd)
		if !found {
			return "", errors.New(file + " has no end marker, so its hand-written part cannot be kept")
		}
		tail = after
	}

	for i, p := range referencePages {
		if p.file == file {
			return p.render(i+1) + "\n\n" + referenceEnd + tail, nil
		}
	}

	return "", errors.New("no CLI reference page " + file)
}

func (p referencePage) render(order int) string {
	blocks := []string{
		"---\ntitle: " + p.title + "\ndescription: " + p.description + "\nsidebar:\n  order: " + strconv.Itoa(order) + "\n---",
		referenceStart,
	}
	if p.verbs == nil {
		return strings.Join(append(blocks, overview()...), "\n\n")
	}

	for _, verb := range p.verbs {
		blocks = append(blocks, verbSection(verb, "##")...)
		cmd, _ := lookup(verb)
		for _, sub := range cmd.subs {
			blocks = append(blocks, verbSection(verb+" "+sub.name, "###")...)
		}
	}

	return strings.Join(blocks, "\n\n")
}

// overview is the top-level help, with each verb linked to its section on its group's page.
func overview() []string {
	h := helps[""]
	blocks := []string{prose(h.opening()), usageBlock(h.usage)}
	for _, n := range h.notes {
		blocks = append(blocks, n.markdown())
	}
	blocks = append(blocks, prose("Run 'shard COMMAND --help' for options and examples."), "## Commands")

	pageOf := map[string]string{}
	for _, p := range referencePages {
		for _, verb := range p.verbs {
			pageOf[verb] = strings.TrimSuffix(p.file, ".mdx")
		}
	}
	for _, group := range verbGroups {
		rows := make([][]string, 0, len(group.verbs))
		for _, verb := range group.verbs {
			link := "[`shard " + verb + "`](/docs/reference/cli/" + pageOf[verb] + "/#" + anchor(verb) + ")"
			rows = append(rows, []string{link, tableCell(helps[verb].summary)})
		}
		blocks = append(blocks, "**"+group.title+"**", mdTable([]string{"Command", "Description"}, rows))
	}

	envRows := make([][]string, 0, len(h.env))
	for _, r := range h.env {
		envRows = append(envRows, []string{code(r.left), tableCell(r.text)})
	}

	return append(blocks,
		"## Global options", flagTable(h.flags),
		"## Environment variables", mdTable([]string{"Variable", "Description"}, envRows),
	)
}

// verbSection is one verb's help as a page section, examples ahead of the reference so it opens with something to run.
func verbSection(key, heading string) []string {
	h := helps[key]
	blocks := []string{heading + " shard " + key, prose(h.opening())}
	if len(h.intro) > 0 {
		blocks = append(blocks, prose(strings.Join(h.intro, " ")))
	}
	blocks = append(blocks, usageBlock(h.usage))
	if len(h.examples) > 0 {
		title := "**Examples**"
		if len(h.examples) == 1 {
			title = "**Example**"
		}
		blocks = append(blocks, title, "```sh\n"+strings.Join(h.examples, "\n")+"\n```")
	}

	cmd, _ := lookup(key)
	if len(cmd.subs) > 0 {
		rows := make([][]string, 0, len(cmd.subs))
		for _, r := range subRows(cmd) {
			sub := key + " " + r.left
			rows = append(rows, []string{"[`shard " + sub + "`](#" + anchor(sub) + ")", tableCell(r.text)})
		}
		blocks = append(blocks, "**Commands**", mdTable([]string{"Command", "Description"}, rows))
	}
	if len(h.args) > 0 {
		rows := make([][]string, 0, len(h.args))
		for _, r := range h.args {
			rows = append(rows, []string{code(r.left), tableCell(r.text)})
		}
		blocks = append(blocks, "**Arguments**", mdTable([]string{"Argument", "Description"}, rows))
	}
	if len(h.flags) > 0 {
		blocks = append(blocks, "**Options**", flagTable(h.flags))
	}
	for _, n := range h.notes {
		blocks = append(blocks, n.markdown())
	}

	return blocks
}

// flagTable has a Default column only when a flag has a default to show.
func flagTable(flags []flagHelp) string {
	withDefault := false
	for _, f := range flags {
		withDefault = withDefault || f.def != ""
	}

	rows := make([][]string, 0, len(flags))
	for _, f := range flags {
		r := []string{code(f.spell), tableCell(f.text)}
		if withDefault && f.def != "" {
			r = append(r, code(f.def))
		}
		if withDefault && f.def == "" {
			r = append(r, "")
		}
		rows = append(rows, r)
	}
	if withDefault {
		return mdTable([]string{"Option", "Description", "Default"}, rows)
	}

	return mdTable([]string{"Option", "Description"}, rows)
}

// markdown renders a paragraph note as one paragraph, and a titled one as its title, its rows as a list and its lines.
func (n note) markdown() string {
	if n.title == "" {
		return prose(strings.Join(n.lines, " "))
	}

	blocks := []string{"**" + n.title + "**"}
	if len(n.rows) > 0 {
		items := make([]string, 0, len(n.rows))
		for _, r := range n.rows {
			items = append(items, "- "+code(r.left)+": "+prose(r.text))
		}
		blocks = append(blocks, strings.Join(items, "\n"))
	}

	// A command or an indented line is code, and a run of other lines is one paragraph.
	var text, codeLines []string
	flush := func() {
		if len(text) > 0 {
			blocks = append(blocks, prose(strings.Join(text, " ")))
		}
		if len(codeLines) > 0 {
			lang := "text"
			if strings.HasPrefix(codeLines[0], "shard ") {
				lang = "sh"
			}
			blocks = append(blocks, "```"+lang+"\n"+strings.Join(codeLines, "\n")+"\n```")
		}
		text, codeLines = nil, nil
	}
	for _, line := range n.lines {
		isCode := strings.HasPrefix(line, " ") || strings.HasPrefix(line, "shard ")
		if line == "" || !isCode && len(codeLines) > 0 || isCode && len(text) > 0 {
			flush()
		}
		if isCode {
			codeLines = append(codeLines, strings.TrimSpace(line))
		}
		if !isCode && line != "" {
			text = append(text, line)
		}
	}
	flush()

	return strings.Join(blocks, "\n\n")
}

func usageBlock(usage []string) string {
	lines := make([]string, 0, len(usage))
	for _, u := range usage {
		lines = append(lines, "shard "+u)
	}

	return "```text\n" + strings.Join(lines, "\n") + "\n```"
}

func mdTable(head []string, rows [][]string) string {
	lines := []string{"| " + strings.Join(head, " | ") + " |", strings.TrimSuffix(strings.Repeat("| --- ", len(head)), " ") + " |"}
	for _, r := range rows {
		lines = append(lines, strings.TrimRight("| "+strings.Join(r, " | ")+" |", " "))
	}

	return strings.Join(lines, "\n")
}

// anchor is the id the site gives the heading of a verb's section.
func anchor(key string) string { return "shard-" + strings.ReplaceAll(key, " ", "-") }

// code is a code span a table cell can hold, since a pipe ends a cell even inside one.
func code(s string) string { return "`" + strings.ReplaceAll(s, "|", `\|`) + "`" }

// tableCell is prose on one line that a table cell can hold.
func tableCell(text string) string {
	return strings.ReplaceAll(prose(strings.ReplaceAll(text, "\n", " ")), "|", `\|`)
}

var (
	// quoted is a 'span' the help quotes, and never the apostrophe of a possessive.
	quoted = regexp.MustCompile(`(^|[\s(])'([^'\s][^']*)'([\s.,;:)]|$)`)
	// literal is a flag or a variable the help names in a sentence.
	literal = regexp.MustCompile(`(^|[\s(])(--[a-z][a-z0-9-]*|\$[A-Z][A-Z0-9_]*|SHARD_[A-Z_]+)`)
	// product is the name the help capitalizes, which the site writes in lower case.
	product      = regexp.MustCompile(`\bShard\b`)
	markdownMark = strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "<", `\<`, ">", `\>`, "{", `\{`, "}", `\}`, "[", `\[`, "]", `\]`)
)

// prose turns a help sentence into Markdown: what it quotes or names is code, and the rest is escaped so MDX reads it as text.
func prose(text string) string {
	text = product.ReplaceAllString(text, "shard")
	text = untilStable(text, func(s string) string { return quoted.ReplaceAllString(s, "$1`$2`$3") })

	parts := strings.Split(text, "`")
	for i := 0; i < len(parts); i += 2 {
		parts[i] = untilStable(parts[i], func(s string) string { return literal.ReplaceAllString(s, "$1`$2`") })
	}
	parts = strings.Split(strings.Join(parts, "`"), "`")
	for i := 0; i < len(parts); i += 2 {
		parts[i] = markdownMark.Replace(parts[i])
	}

	return strings.Join(parts, "`")
}

// untilStable applies f until it changes nothing, since a match consumes the space the next one starts with.
func untilStable(s string, f func(string) string) string {
	for next := f(s); next != s; next = f(s) {
		s = next
	}

	return s
}
