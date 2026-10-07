// Package apiref writes the REST API reference pages of the site from the spec the routes make.
package apiref

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/presmihaylov/shard/services/api"
)

// Dir is where make openapi writes the REST API reference, relative to the repository root.
const Dir = "website/src/content/docs/docs/reference/api"

const (
	start = "{/* make openapi writes this part from the routes. Edit the routes, not this part. */}"
	// end closes what make openapi writes; a page keeps the hand-written text above start and below end.
	end          = "{/* make openapi writes everything above this line. Write by hand below it. */}"
	baseURL      = "/docs/reference/api/"
	schemaPrefix = "#/components/schemas/"
)

// page is one page of the reference: the routes of one tag, or the overview when tag is empty.
type page struct {
	file, title, description, tag string
}

// pages are the sidebar entries in order, so a page's position is its sidebar order and an object lives on the first page that uses it.
var pages = []page{
	{"index.mdx", "Overview", "The shard REST API, with how to reach it, its tokens, every route and the error body.", ""},
	{"sandboxes.mdx", "Sandboxes", "The routes that create and manage sandboxes, read their output and egress decisions, grant secrets and attach policies.", "sandboxes"},
	{"files.mdx", "Files", "The routes that copy files and directories into and out of a running sandbox.", "files"},
	{"exec.mdx", "Exec", "The routes that run a command in a running sandbox, attach to it, signal it and resize its terminal.", "exec"},
	{"snapshots.mdx", "Snapshots", "The routes that save a stopped sandbox's files as a snapshot and manage snapshots.", "snapshots"},
	{"policies.mdx", "Policies", "The routes that store the network policies a sandbox can attach.", "policies"},
	{"secrets.mdx", "Secrets", "The routes that store secrets and their destinations, and never answer a value.", "secrets"},
	{"meta.mdx", "Meta", "The routes that read the daemon version, the verbs its provider supports and the scopes a token can carry.", "meta"},
	{"app.mdx", "App", "The routes that attach to and stop the app of a sandbox created with a command.", "app"},
	{"ports.mdx", "Ports", "The routes that forward a host port to a port inside a sandbox, list the forwards and remove them.", "ports"},
}

// Files names every page make openapi writes, in sidebar order.
func Files() []string {
	files := make([]string, 0, len(pages))
	for _, p := range pages {
		files = append(files, p.file)
	}

	return files
}

// Page renders one page from the spec and keeps what current holds by hand above the start marker and below the end marker.
func Page(file, current string) (string, error) {
	head, tail := "\n", "\n"
	if current != "" {
		_, rest, fenced := strings.Cut(strings.TrimPrefix(current, "---\n"), "\n---\n")
		before, _, started := strings.Cut(rest, start)
		_, after, ended := strings.Cut(rest, end)
		if !fenced || !started || !ended {
			return "", errors.New(file + " lacks its frontmatter or a marker, so its hand-written part cannot be kept")
		}
		head, tail = before, after
	}

	r, err := newRenderer()
	if err != nil {
		return "", err
	}
	for i, p := range pages {
		if p.file != file {
			continue
		}
		body, err := r.render(p)
		if err != nil {
			return "", fmt.Errorf("%s: %w", file, err)
		}
		front := "---\ntitle: " + p.title + "\ndescription: " + p.description + "\nsidebar:\n  order: " + strconv.Itoa(i+1) + "\n---\n"

		return front + head + start + "\n\n" + body + "\n\n" + end + tail, nil
	}

	return "", errors.New("no REST API reference page " + file)
}

type specDoc struct {
	Paths      map[string]map[string]*specOp `json:"paths"`
	Components struct {
		Schemas map[string]*schema `json:"schemas"`
	} `json:"components"`
}

type specOp struct {
	Summary     string               `json:"summary"`
	Description string               `json:"description"`
	Tags        []string             `json:"tags"`
	Parameters  []param              `json:"parameters"`
	RequestBody *body                `json:"requestBody"`
	Responses   map[string]*response `json:"responses"`
}

type param struct {
	Name        string  `json:"name"`
	In          string  `json:"in"`
	Description string  `json:"description"`
	Required    bool    `json:"required"`
	Schema      *schema `json:"schema"`
}

type body struct {
	Required bool              `json:"required"`
	Content  map[string]*media `json:"content"`
}

type media struct {
	Schema *schema `json:"schema"`
}

type response struct {
	Description string             `json:"description"`
	Content     map[string]*media  `json:"content"`
	Headers     map[string]*header `json:"headers"`
	Messages    map[string]*schema `json:"x-shard-messages"`
}

type header struct {
	Description string  `json:"description"`
	Schema      *schema `json:"schema"`
}

type schema struct {
	Ref         string             `json:"$ref"`
	Type        types              `json:"type"`
	Format      string             `json:"format"`
	Description string             `json:"description"`
	Enum        []string           `json:"enum"`
	Items       *schema            `json:"items"`
	Properties  map[string]*schema `json:"properties"`
	Required    []string           `json:"required"`
	AnyOf       []*schema          `json:"anyOf"`
	Minimum     *int64             `json:"minimum"`
	Maximum     *int64             `json:"maximum"`
	MinLength   *int64             `json:"minLength"`
	MinItems    *int64             `json:"minItems"`
}

// types is a schema's type, which OpenAPI 3.1 writes as one name or as a list of them.
type types []string

func (t *types) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var one string
		if err := json.Unmarshal(b, &one); err != nil {
			return fmt.Errorf("decode a schema type: %w", err)
		}
		*t = types{one}

		return nil
	}

	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return fmt.Errorf("a schema type is neither a name nor a list of names: %w", err)
	}
	*t = many

	return nil
}

// route is one public route with the operation the spec gives it.
type route struct {
	api.Route
	op *specOp
}

// renderer holds the spec, the public routes of each tag in the order the daemon registers them, and the page each object lives on.
type renderer struct {
	spec  specDoc
	byTag map[string][]route
	home  map[string]string
}

func newRenderer() (*renderer, error) {
	raw, err := api.Spec()
	if err != nil {
		return nil, err
	}
	r := &renderer{byTag: map[string][]route{}, home: map[string]string{}}
	if err := json.Unmarshal(raw, &r.spec); err != nil {
		return nil, fmt.Errorf("decode the spec: %w", err)
	}

	for _, rt := range api.Routes() {
		if rt.Class != api.Public {
			continue
		}
		op := r.spec.Paths[rt.Pattern][strings.ToLower(rt.Method)]
		if op == nil {
			return nil, fmt.Errorf("the spec has no operation for %s %s", rt.Method, rt.Pattern)
		}
		if len(op.Tags) != 1 {
			return nil, fmt.Errorf("%s %s has tags %v, want exactly one", rt.Method, rt.Pattern, op.Tags)
		}
		r.byTag[op.Tags[0]] = append(r.byTag[op.Tags[0]], route{rt, op})
	}

	for _, p := range pages {
		for _, name := range r.objectsOf(p) {
			if _, ok := r.home[name]; !ok {
				r.home[name] = p.file
			}
		}
	}

	return r, nil
}

// objectsOf names every object a page's routes use, directly or through another object; the overview uses the error body.
func (r *renderer) objectsOf(p page) []string {
	seen := map[string]bool{}
	var visit func(s *schema)
	visit = func(s *schema) {
		if s == nil {
			return
		}
		if name, ok := strings.CutPrefix(s.Ref, schemaPrefix); ok {
			if seen[name] {
				return
			}
			seen[name] = true
			s = r.spec.Components.Schemas[name]
			if s == nil {
				return
			}
		}
		visit(s.Items)
		for _, a := range s.AnyOf {
			visit(a)
		}
		for _, prop := range s.Properties {
			visit(prop)
		}
	}

	if p.tag == "" {
		visit(&schema{Ref: schemaPrefix + "Error"})
	}
	for _, rt := range r.byTag[p.tag] {
		for _, s := range rt.op.schemas() {
			visit(s)
		}
	}

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	slices.Sort(names)

	return names
}

// schemas are the schemas an operation names directly, less the error body every route shares.
func (op *specOp) schemas() []*schema {
	var out []*schema
	for _, p := range op.Parameters {
		out = append(out, p.Schema)
	}
	if op.RequestBody != nil {
		for _, m := range op.RequestBody.Content {
			out = append(out, m.Schema)
		}
	}
	for status, resp := range op.Responses {
		if status == "default" {
			continue
		}
		for _, m := range resp.Content {
			out = append(out, m.Schema)
		}
		for _, h := range resp.Headers {
			out = append(out, h.Schema)
		}
		for _, s := range resp.Messages {
			out = append(out, s)
		}
	}

	return out
}

func (r *renderer) render(p page) (string, error) {
	var blocks []string
	if p.tag == "" {
		blocks = append(blocks, "## Routes")
		for _, tagged := range pages[1:] {
			blocks = append(blocks, "**"+tagged.title+"**", r.routeTable(tagged))
		}
	}
	if p.tag != "" {
		blocks = append(blocks, "Each route answers an error with the "+r.link("Error", p.file)+" body, unless its responses say otherwise.")
		for _, rt := range r.byTag[p.tag] {
			blocks = append(blocks, r.operation(p, rt)...)
		}
	}

	var objects []string
	for _, name := range r.objectsOf(p) {
		if r.home[name] == p.file {
			objects = append(objects, r.object(p, name)...)
		}
	}
	if len(objects) > 0 {
		blocks = append(append(blocks, "## Objects"), objects...)
	}

	if err := uniqueAnchors(blocks); err != nil {
		return "", err
	}

	return strings.Join(blocks, "\n\n"), nil
}

func (r *renderer) routeTable(p page) string {
	rows := make([][]string, 0, len(r.byTag[p.tag]))
	for _, rt := range r.byTag[p.tag] {
		title := heading(rt.op.Summary)
		link := "[" + escape(title) + "](" + baseURL + strings.TrimSuffix(p.file, ".mdx") + "/#" + anchor(title) + ")"
		rows = append(rows, []string{code(rt.Method), code(rt.Pattern), scope(rt.Scope), link})
	}

	return table([]string{"Method", "Path", "Scope", "Description"}, rows)
}

// operation is one route as a section: its line, what the spec says of it, then its parameters, body and answers.
func (r *renderer) operation(p page, rt route) []string {
	op := rt.op
	blocks := []string{"## " + escape(heading(op.Summary)), code(rt.Method+" "+rt.Pattern) + " · scope " + scope(rt.Scope)}
	if op.Description != "" {
		blocks = append(blocks, prose(op.Description))
	}

	if len(op.Parameters) > 0 {
		rows := make([][]string, 0, len(op.Parameters))
		for _, prm := range op.Parameters {
			rows = append(rows, []string{code(prm.Name), prm.In, r.typeOf(prm.Schema, p.file), yes(prm.Required), cell(prm.Description + constraints(prm.Schema))})
		}
		blocks = append(blocks, "**Parameters**", describedTable([]string{"Name", "In", "Type", "Required", "Description"}, rows))
	}

	if op.RequestBody != nil {
		need := "optional"
		if op.RequestBody.Required {
			need = "required"
		}
		blocks = append(blocks, "**Request body**", r.contents(op.RequestBody.Content, p.file)+", "+need+".")
	}

	var rows, headers, messages [][]string
	for _, status := range sortedKeys(op.Responses) {
		resp := op.Responses[status]
		if status == "default" && isSharedError(resp) {
			continue
		}
		label, content, text := status, r.contents(resp.Content, p.file), resp.Description
		if status == "default" {
			label = "Error"
		}
		if n, err := strconv.Atoi(status); err == nil && http.StatusText(n) == text {
			text = ""
		}
		if status == "101" {
			content = "WebSocket"
		}
		if content == "" {
			content = "none"
		}
		rows = append(rows, []string{label, content, cell(text)})

		for _, name := range sortedKeys(resp.Headers) {
			h := resp.Headers[name]
			headers = append(headers, []string{code(name), status, r.typeOf(h.Schema, p.file), cell(h.Description)})
		}
		for _, stream := range sortedKeys(resp.Messages) {
			messages = append(messages, []string{stream, r.typeOf(resp.Messages[stream], p.file)})
		}
	}
	blocks = append(blocks, "**Responses**", describedTable([]string{"Status", "Body", "Description"}, rows))
	if len(headers) > 0 {
		blocks = append(blocks, "**Response headers**", describedTable([]string{"Header", "Status", "Type", "Description"}, headers))
	}
	if len(messages) > 0 {
		blocks = append(blocks, "**WebSocket messages**", table([]string{"Stream byte", "Payload"}, messages))
	}

	return blocks
}

// isSharedError is the default answer every route has, which the overview documents once.
func isSharedError(resp *response) bool {
	m := resp.Content["application/json"]

	return resp.Description == "Error" && len(resp.Content) == 1 && m != nil && m.Schema != nil && m.Schema.Ref == schemaPrefix+"Error"
}

// contents is a body in each media type it comes in.
func (r *renderer) contents(content map[string]*media, from string) string {
	parts := make([]string, 0, len(content))
	for _, mt := range sortedKeys(content) {
		parts = append(parts, r.typeOf(content[mt].Schema, from)+" as "+code(mt))
	}

	return strings.Join(parts, ", or ")
}

// object is the section of one object: a row per field, in name order.
func (r *renderer) object(p page, name string) []string {
	s := r.spec.Components.Schemas[name]
	rows := make([][]string, 0, len(s.Properties))
	for _, field := range sortedKeys(s.Properties) {
		prop := s.Properties[field]
		rows = append(rows, []string{code(field), r.typeOf(prop, p.file), yes(slices.Contains(s.Required, field)), cell(prop.Description + constraints(prop))})
	}

	return []string{"### " + name, describedTable([]string{"Field", "Type", "Required", "Description"}, rows)}
}

// typeOf names a schema's type, with a link to the object it refers to.
func (r *renderer) typeOf(s *schema, from string) string {
	if s == nil {
		return ""
	}
	if name, ok := strings.CutPrefix(s.Ref, schemaPrefix); ok {
		return r.link(name, from)
	}
	if len(s.AnyOf) == 2 && slices.Equal(s.AnyOf[1].Type, types{"null"}) {
		return r.typeOf(s.AnyOf[0], from) + " or null"
	}

	var names []string
	for _, t := range s.Type {
		switch {
		case t == "null":
			continue
		case t == "array":
			names = append(names, "array of "+r.typeOf(s.Items, from))
		case s.Format == "binary":
			names = append(names, "binary")
		case s.Format != "":
			names = append(names, t+" ("+s.Format+")")
		default:
			names = append(names, t)
		}
	}
	if slices.Contains(s.Type, "null") {
		names = append(names, "null")
	}

	return strings.Join(names, " or ")
}

// link points at the section of an object on the page that documents it.
func (r *renderer) link(name, from string) string {
	home := r.home[name]
	if home == from {
		return "[" + name + "](#" + anchor(name) + ")"
	}

	path := strings.TrimSuffix(strings.TrimSuffix(home, ".mdx"), "index")
	if path != "" {
		path += "/"
	}

	return "[" + name + "](" + baseURL + path + "#" + anchor(name) + ")"
}

// constraints are a schema's enum and bounds as sentences, which lead with a space so they follow a description.
func constraints(s *schema) string {
	if s == nil {
		return ""
	}
	var out []string
	if len(s.Enum) > 0 {
		values := make([]string, 0, len(s.Enum))
		for _, v := range s.Enum {
			values = append(values, "`"+v+"`")
		}
		out = append(out, "One of "+strings.Join(values, ", ")+".")
	}
	switch {
	case s.Minimum != nil && s.Maximum != nil:
		out = append(out, fmt.Sprintf("From %d to %d.", *s.Minimum, *s.Maximum))
	case s.Minimum != nil:
		out = append(out, fmt.Sprintf("At least %d.", *s.Minimum))
	case s.Maximum != nil:
		out = append(out, fmt.Sprintf("At most %d.", *s.Maximum))
	}
	if s.MinLength != nil {
		out = append(out, atLeast(*s.MinLength, "character"))
	}
	if s.MinItems != nil {
		out = append(out, atLeast(*s.MinItems, "item"))
	}
	if len(out) == 0 {
		return ""
	}

	return " " + strings.Join(out, " ")
}

func atLeast(n int64, noun string) string {
	if n != 1 {
		noun += "s"
	}

	return fmt.Sprintf("At least %d %s.", n, noun)
}

// uniqueAnchors refuses a page where two headings would get one id, since the site would then number the second.
func uniqueAnchors(blocks []string) error {
	seen := map[string]bool{}
	for _, b := range blocks {
		text, ok := strings.CutPrefix(b, "## ")
		if !ok {
			text, ok = strings.CutPrefix(b, "### ")
		}
		if !ok {
			continue
		}
		id := anchor(strings.ReplaceAll(text, `\`, ""))
		if seen[id] {
			return fmt.Errorf("two headings have the id %q", id)
		}
		seen[id] = true
	}

	return nil
}

func scope(s api.Scope) string {
	if s == api.AnyToken {
		return "any token"
	}

	return code(string(s))
}

func yes(b bool) string {
	if b {
		return "yes"
	}

	return "no"
}

// heading is a summary as a title; some summaries start in lower case.
func heading(summary string) string {
	r := []rune(summary)
	if len(r) == 0 {
		return summary
	}
	r[0] = unicode.ToUpper(r[0])

	return string(r)
}

// anchor is the id the site gives a heading: github-slugger lower-cases it, drops punctuation and turns each space into a hyphen.
func anchor(text string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(text) {
		switch {
		case c == ' ':
			b.WriteRune('-')
		case c == '-' || c == '_' || unicode.IsLetter(c) || unicode.IsDigit(c) || unicode.IsMark(c):
			b.WriteRune(c)
		}
	}

	return b.String()
}

func table(head []string, rows [][]string) string {
	lines := []string{"| " + strings.Join(head, " | ") + " |", strings.TrimSuffix(strings.Repeat("| --- ", len(head)), " ") + " |"}
	for _, row := range rows {
		lines = append(lines, strings.TrimRight("| "+strings.Join(row, " | ")+" |", " "))
	}

	return strings.Join(lines, "\n")
}

// describedTable drops the last column, the description, when no row has one.
func describedTable(head []string, rows [][]string) string {
	last := len(head) - 1
	for _, row := range rows {
		if row[last] != "" {
			return table(head, rows)
		}
	}

	trimmed := make([][]string, 0, len(rows))
	for _, row := range rows {
		trimmed = append(trimmed, row[:last])
	}

	return table(head[:last], trimmed)
}

// code is a code span a table cell can hold, since a pipe ends a cell even inside one.
func code(s string) string { return "`" + strings.ReplaceAll(s, "|", `\|`) + "`" }

// cell is prose that a table cell can hold.
func cell(text string) string {
	return strings.ReplaceAll(prose(strings.TrimSpace(text)), "|", `\|`)
}

var markdownMark = strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "<", `\<`, ">", `\>`, "{", `\{`, "}", `\}`, "[", `\[`, "]", `\]`)

// escape makes text read as text in MDX, where a brace opens an expression and an angle bracket a tag.
func escape(text string) string { return markdownMark.Replace(text) }

// prose escapes a spec sentence outside the code spans it holds, so the constraints keep their backticks.
func prose(text string) string {
	parts := strings.Split(text, "`")
	for i := 0; i < len(parts); i += 2 {
		parts[i] = escape(parts[i])
	}

	return strings.Join(parts, "`")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	return keys
}
