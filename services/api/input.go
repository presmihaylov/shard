package api

// The inputs of the public routes: Huma reads each path and query param off its tag, checks it, and decodes Body.

type sandboxPath struct {
	ID string `path:"id" doc:"The sandbox id or name."`
}

type sandboxBody[B any] struct {
	ID   string `path:"id" doc:"The sandbox id or name."`
	Body *B
}

// sandboxRequest is a body the verb refuses to go without; a value Body makes Huma refuse an empty one and the spec require it.
type sandboxRequest[B any] struct {
	ID   string `path:"id" doc:"The sandbox id or name."`
	Body B
}

type execPath struct {
	ID   string `path:"id" doc:"The sandbox id or name."`
	Exec string `path:"exec" doc:"The exec id."`
}

type execBody[B any] struct {
	ID   string `path:"id" doc:"The sandbox id or name."`
	Exec string `path:"exec" doc:"The exec id."`
	Body *B
}

type namePath struct {
	Name string `path:"name"`
}

type nameBody[B any] struct {
	Name string `path:"name"`
	Body *B
}

type nameRequest[B any] struct {
	Name string `path:"name"`
	Body B
}

type refPath struct {
	Ref string `path:"ref" doc:"The snapshot id or name."`
}

type bodyRequest[B any] struct {
	Body B
}

type filePath struct {
	ID   string `path:"id" doc:"The sandbox id or name."`
	Path string `query:"path" required:"true" doc:"The absolute path inside the sandbox."`
}

type followInput struct {
	ID     string `path:"id" doc:"The sandbox id or name."`
	Follow bool   `query:"follow" doc:"Keep the stream open until the sandbox stops or is removed; a WebSocket upgrade requires follow=true."`
}
