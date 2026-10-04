package models

// Scope is one permission a token can carry, and what it lets the token do.
type Scope struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

const (
	ScopeSandboxRead   = "sandbox:read"
	ScopeSandboxWrite  = "sandbox:write"
	ScopeSandboxDelete = "sandbox:delete"
	ScopeExec          = "exec"
	ScopeSecret        = "secret:*"
	ScopePolicy        = "policy:*"
	ScopeAll           = "*"
)

// Scopes is the one table that token validation, help and discovery read, so a valid scope is always a listed one.
var Scopes = []Scope{
	{ScopeSandboxRead, "View sandboxes, logs and snapshots"},
	{ScopeSandboxWrite, "Create sandboxes, change their state and create snapshots"},
	{ScopeSandboxDelete, "Remove sandboxes and snapshots"},
	{ScopeExec, "Run commands and access sandbox files"},
	{ScopeSecret, "Manage secrets and secret grants"},
	{ScopePolicy, "Manage policies and their sandbox assignments"},
	{ScopeAll, "All available permissions"},
}
