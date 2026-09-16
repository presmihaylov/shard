package models

// Code is the half of an API error body a program reads; the table in docs/daemon.md says when each is answered.
type Code string

const (
	CodeInvalidRequest    Code = "invalid_request"
	CodeNotFound          Code = "not_found"
	CodeSandboxNotRunning Code = "sandbox_not_running"
	CodeSandboxNotStopped Code = "sandbox_not_stopped"
	CodeSandboxNotPaused  Code = "sandbox_not_paused"
	CodeSandboxLive       Code = "sandbox_live"
	CodeNoSnapshot        Code = "no_snapshot"
	CodeUnsupported       Code = "unsupported"
	CodeInUse             Code = "in_use"
	CodeWebSocketRequired Code = "websocket_required"
	CodeUnauthorized      Code = "unauthorized"
	CodeInternal          Code = "internal"
)
