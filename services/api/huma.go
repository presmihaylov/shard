package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"reflect"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/services/egress"
)

func init() {
	huma.NewError = newError
	// A list the daemon answers is never null, so the spec says so.
	huma.DefaultArrayNullable = false
}

// apiError is every refusal, one object under error and nothing else at the root; Huma writes it under status.
type apiError struct {
	Object ErrorObject `json:"error"`
	status int
	// cause is what the message left out, which only the daemon log reads.
	cause error
}

func (e *apiError) Error() string { return e.Object.Message }

func (e *apiError) GetStatus() int { return e.status }

// fail is the error a typed handler returns, which Huma answers with the status and the code its type says; every typed route is public.
func fail(err error) error {
	return refusal(err, false)
}

// newError answers Huma's own refusals in the daemon's shape: a bad request is invalid_request whatever status Huma picked.
func newError(status int, msg string, errs ...error) huma.StatusError {
	if status == http.StatusRequestEntityTooLarge {
		return &apiError{status: status, Object: ErrorObject{Code: models.CodeBodyTooLarge, Message: bodyTooLarge}}
	}
	if status >= http.StatusInternalServerError {
		return &apiError{status: status, Object: ErrorObject{Code: models.CodeInternal, Message: internalText}, cause: errors.New(withDetails(msg, errs))}
	}

	return &apiError{status: http.StatusBadRequest, Object: ErrorObject{Code: models.CodeInvalidRequest, Message: withDetails(msg, errs)}}
}

// withDetails names each refusal by its message and location; ErrorDetail.Error() prints the value, which on a secret PUT is the secret.
func withDetails(msg string, errs []error) string {
	details := make([]string, 0, len(errs))
	for _, err := range errs {
		var detailer huma.ErrorDetailer
		if !errors.As(err, &detailer) {
			details = append(details, err.Error())

			continue
		}

		d := detailer.ErrorDetail()
		if d.Location == "" {
			details = append(details, d.Message)

			continue
		}
		details = append(details, d.Message+" ("+d.Location+")")
	}

	if len(details) == 0 {
		return msg
	}

	return msg + ": " + strings.Join(details, "; ")
}

// format is the one wire format: json.Marshal as writeJSON writes it, with no trailing newline.
func (h *Handler) format() huma.Format {
	return huma.Format{
		Marshal: func(w io.Writer, v any) error {
			body, err := json.Marshal(v)
			if err != nil {
				return fmt.Errorf("encode the response: %w", err)
			}

			// The status is on its way, so the one thing left to do with a client that hung up is to say so.
			if _, err := w.Write(body); err != nil {
				h.log.Printf("api: write the response: %v", err)
			}

			return nil
		},
		Unmarshal: json.Unmarshal,
	}
}

// config leaves CreateHooks empty, so no $schema field or Link header reaches the wire, and sets no path, so the daemon serves no spec.
func (h *Handler) config() huma.Config {
	return huma.Config{
		OpenAPI: &huma.OpenAPI{
			OpenAPI: "3.1.0",
			Info: &huma.Info{
				Title:       "shard",
				Version:     APIVersion,
				Description: "The public routes of the shard daemon. shard serve checks a bearer token and its scope, named per operation as x-shard-scope; the daemon socket takes no token.",
			},
			Components: &huma.Components{
				Schemas:         huma.NewMapRegistry("#/components/schemas/", schemaName),
				SecuritySchemes: map[string]*huma.SecurityScheme{"bearer": {Type: "http", Scheme: "bearer"}},
			},
			Security: []map[string][]string{{"bearer": {}}},
		},
		Formats:       map[string]huma.Format{"application/json": h.format()},
		Transformers:  []huma.Transformer{h.logRefusal},
		DefaultFormat: "application/json",
	}
}

// logRefusal keeps the cause a typed route's public text left out, as errorBody does for a raw one.
func (h *Handler) logRefusal(ctx huma.Context, _ string, v any) (any, error) {
	if refused, ok := v.(*apiError); ok {
		h.logCause(ctx.Method()+" "+ctx.URL().Path, refused.cause)
	}

	return v, nil
}

// schemaName keeps Huma's names, less apiError, which an SDK would read as ApiError, and egress.Record, which the API calls a decision.
func schemaName(t reflect.Type, hint string) string {
	if t == reflect.TypeFor[apiError]() {
		return "Error"
	}
	if t == reflect.TypeFor[egress.Record]() {
		return "EgressDecision"
	}

	return huma.DefaultSchemaNamer(t, hint)
}

// adapter registers each operation on the daemon's own mux, so a public route and a local one share one pattern table.
type adapter struct {
	mux *http.ServeMux
}

func (a adapter) Handle(op *huma.Operation, handler func(huma.Context)) {
	a.mux.HandleFunc(op.Method+" "+op.Path, func(w http.ResponseWriter, r *http.Request) {
		handler(humago.NewContext(op, prepare(r), w))
	})
}

func (a adapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mux.ServeHTTP(w, r)
}

// scopesKey holds the scopes the front stamped, so a typed handler reads them without the request.
type scopesKey struct{}

// prepare keeps what the decoder did before Huma: a body is JSON whatever its Content-Type, and a bare ?flag is an empty value.
func prepare(r *http.Request) *http.Request {
	ctx := r.Context()
	if scopes, stamped := stampedScopes(r.Header); stamped {
		ctx = context.WithValue(ctx, scopesKey{}, scopes)
	}

	r = r.Clone(ctx)
	r.Header.Del("Content-Type")
	r.URL.RawQuery = r.URL.Query().Encode()

	return r
}

// endpoint registers one public route's handler under the operation the route table built for it.
type endpoint func(api huma.API, op huma.Operation)

// typed is a route whose input and answer Huma reads off the handler's types.
func typed[I, O any](handler func(context.Context, *I) (*O, error)) endpoint {
	return func(api huma.API, op huma.Operation) {
		huma.Register(api, op, handler)
	}
}

// rawReply hands the response to a net/http handler, for an answer Huma cannot type: a stream, a file or an upgrade.
type rawReply struct {
	Body func(huma.Context)
}

// raw is a route whose params Huma checks against I, and whose body and answer the handler reads and writes itself, so a file is never buffered.
func raw[I any](handler http.HandlerFunc, describe func(huma.Registry, *huma.Operation)) endpoint {
	return documented(describe, typed(func(context.Context, *I) (*rawReply, error) {
		return &rawReply{Body: func(ctx huma.Context) {
			r, w := humago.Unwrap(ctx)
			handler(w, r)
		}}, nil
	}))
}

// documented adds what Huma cannot read off a handler's types, the answers a raw body writes, before the route registers.
func documented(describe func(huma.Registry, *huma.Operation), serve endpoint) endpoint {
	return func(api huma.API, op huma.Operation) {
		describe(api.OpenAPI().Components.Schemas, &op)
		serve(api, op)
	}
}

// reply is a typed answer, which Huma writes as JSON under the operation's status.
type reply[T any] struct {
	Body T
}

// answer is the reply to a verb that returns a value, or the refusal its error says.
func answer[T any](body T, err error) (*reply[T], error) {
	if err != nil {
		return nil, fail(err)
	}

	return &reply[T]{Body: body}, nil
}

// done is the reply to a verb with no body, which Huma writes as the operation's status, 204.
func done(err error) (*struct{}, error) {
	if err != nil {
		return nil, fail(err)
	}

	return nil, nil
}

// value is the body a client sent, or the zero value for an empty one.
func value[B any](body *B) B {
	if body == nil {
		var zero B

		return zero
	}

	return *body
}

// operation names a public route in the spec; a status of 0 is Huma's default, 200 with a body and 204 without.
func operation(tag, id, summary string, status int) huma.Operation {
	return huma.Operation{OperationID: id, Summary: summary, Tags: []string{tag}, DefaultStatus: status}
}

// register serves every route on mux: a public one through Huma, so the spec describes it, and a local one as a plain handler the spec never sees.
func (h *Handler) register(mux *http.ServeMux) huma.API {
	api := huma.NewAPI(h.config(), adapter{mux: mux})
	registry := api.OpenAPI().Components.Schemas
	errSchema := registry.Schema(reflect.TypeFor[apiError](), true, "")

	for _, e := range h.routeTable() {
		if e.Class == Local {
			mux.HandleFunc(e.Method+" "+e.Pattern, markLocal(e.handler))

			continue
		}

		op := e.op
		op.Method = e.Method
		op.Path = e.Pattern
		op.Extensions = map[string]any{"x-shard-scope": string(e.Scope)}
		// The cap and the timeout are the decoder's and the server's, so a body Huma reads meets the same bounds as before.
		op.MaxBodyBytes = maxBody + 1
		op.BodyReadTimeout = readTimeout
		op.Responses = map[string]*huma.Response{
			"default": {Description: "Error", Content: map[string]*huma.MediaType{"application/json": {Schema: errSchema}}},
		}
		e.serve(api, op)
	}
	// A record carries the backoff its request may leave out, and the two share one field.
	registry.Map()["Restart"].Required = append(registry.Map()["Restart"].Required, "backoff")
	nullStructs(registry)

	// The mux answers an unknown path with a JSON error, like every other error body on this socket.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		h.writeJSON(w, http.StatusNotFound, apiError{Object: ErrorObject{Code: models.CodeNotFound, Message: fmt.Sprintf("no route for %s %s", r.Method, r.URL.Path)}})
	})

	return api
}

// nullStructs lets a pointer-to-struct field without omitempty be null, as an exec's exit_status is before it ends; Huma gives it a bare $ref.
func nullStructs(registry huma.Registry) {
	for name, schema := range registry.Map() {
		t := registry.TypeFromRef("#/components/schemas/" + name)
		if t == nil || t.Kind() != reflect.Struct {
			continue
		}
		nullFields(t, schema)
		schema.PrecomputeMessages()
	}
}

func nullFields(t reflect.Type, schema *huma.Schema) {
	for f := range t.Fields() {
		if f.Anonymous && f.Tag.Get("json") == "" {
			nullFields(deref(f.Type), schema)

			continue
		}
		if !f.IsExported() || f.Type.Kind() != reflect.Pointer || f.Type.Elem().Kind() != reflect.Struct {
			continue
		}

		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" || strings.Contains(opts, "omitempty") || strings.Contains(opts, "omitzero") {
			continue
		}
		if name == "" {
			name = f.Name
		}

		prop, ok := schema.Properties[name]
		if !ok || prop.Ref == "" {
			continue
		}
		nullable := &huma.Schema{AnyOf: []*huma.Schema{{Ref: prop.Ref}, {Type: "null"}}, Description: prop.Description}
		nullable.PrecomputeMessages()
		schema.Properties[name] = nullable
	}
}

func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	return t
}

// Spec is the OpenAPI document of the public routes, which make openapi writes to docs/openapi.json.
func Spec() ([]byte, error) {
	h := &Handler{log: log.New(io.Discard, "", 0)}
	api := h.register(http.NewServeMux())

	var out strings.Builder
	enc := json.NewEncoder(&out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(api.OpenAPI()); err != nil {
		return nil, fmt.Errorf("encode the spec: %w", err)
	}

	return []byte(out.String()), nil
}

// response is one documented answer of a raw route.
func response(description, mediaType string, schema *huma.Schema) *huma.Response {
	return &huma.Response{Description: description, Content: map[string]*huma.MediaType{mediaType: {Schema: schema}}}
}

// upgrade is the 101 a WebSocket handshake gets, whose frames the description names; x-shard-messages maps a stream byte to its JSON payload.
func upgrade(description string, messages map[string]*huma.Schema) *huma.Response {
	if len(messages) == 0 {
		return &huma.Response{Description: description}
	}

	return &huma.Response{Description: description, Extensions: map[string]any{"x-shard-messages": messages}}
}

func schemaOf[T any](registry huma.Registry) *huma.Schema {
	return registry.Schema(reflect.TypeFor[T](), true, "")
}

func binary() *huma.Schema {
	return &huma.Schema{Type: huma.TypeString, Format: "binary"}
}

func text() *huma.Schema {
	return &huma.Schema{Type: huma.TypeString}
}

// statHeader documents X-Shard-Stat, the guest path's stat as JSON.
func statHeader() map[string]*huma.Header {
	return map[string]*huma.Header{StatHeader: {Description: "The guest path's stat as JSON: type is file, dir, symlink or other; size is the logical size in bytes; mode is the permission bits as a number, at most 0o7777; then uid, gid and mtime.", Schema: text()}}
}

// binaryBody is the request body of a raw PUT, which Huma never reads.
func binaryBody(mediaType string) *huma.RequestBody {
	return &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{mediaType: {Schema: binary()}}}
}
