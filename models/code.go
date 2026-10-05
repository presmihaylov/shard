package models

// Code is the half of an API error body a program reads; https://useshards.com/docs/reference/errors/#error-codes says when each is answered.
type Code string

const (
	CodeInvalidRequest    Code = "invalid_request"
	CodeBodyTooLarge      Code = "body_too_large"
	CodeNotFound          Code = "not_found"
	CodeSandboxNotRunning Code = "sandbox_not_running"
	CodeSandboxNotStopped Code = "sandbox_not_stopped"
	CodeSandboxNotPaused  Code = "sandbox_not_paused"
	CodeSandboxLive       Code = "sandbox_live"
	CodeSandboxFailed     Code = "sandbox_failed"
	CodeNoCheckpoint      Code = "no_checkpoint"
	CodeUnsupported       Code = "unsupported"
	CodeInUse             Code = "in_use"
	CodeNameTaken         Code = "name_taken"
	CodeExecExited        Code = "exec_exited"
	CodeExecRunning       Code = "exec_running"
	CodeExecLimit         Code = "exec_limit"
	CodeNoApp             Code = "no_app"
	CodeAppEnded          Code = "app_ended"
	CodeUnauthorized      Code = "unauthorized"
	CodeForbidden         Code = "forbidden"
	CodeTimeout           Code = "timeout"
	CodeCommandNotStarted Code = "command_not_started"
	CodeInternal          Code = "internal"
)
