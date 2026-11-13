// Package jsonrpc is the provider's protocol: JSON-RPC 2.0, one message per
// line, requests on standard input and replies on standard output. Logs go to
// standard error, so a script can read the replies with jq.
package jsonrpc

import (
	"encoding/json"

	"github.com/james00012/cloud-inventory/plugin/core"
)

// Version is the only value the jsonrpc field takes.
const Version = "2.0"

// Request is one call. A message with no id is a notification, which is how a
// list streams a resource without answering the call.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is one reply, carrying either a result or an error.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error carries a kind as well as a message, so a host acts on the kind rather
// than on the text.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Kind    string `json:"kind,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// The error codes this protocol uses. The negative ones are JSON-RPC's own.
const (
	CodeParse          = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603

	// CodeNotEnabled is a call the session was not granted, refused before any
	// cloud call.
	CodeNotEnabled = -32000
)

// The methods a provider answers.
const (
	MethodConfigure      = "configure"
	MethodDescribeTypes  = "describeTypes"
	MethodDiscoverScopes = "discoverScopes"
	MethodAudit          = "audit"
	MethodList           = "list"
	MethodGet            = "get"
	MethodCreate         = "create"
	MethodDelete         = "delete"

	// MethodResource is the notification a list streams one resource on.
	MethodResource = "resource"
)

type ConfigureParams struct {
	Features      []string        `json:"features,omitempty"`
	ExpectAccount string          `json:"expectAccount,omitempty"`
	Caller        string          `json:"caller,omitempty"`
	Run           string          `json:"run,omitempty"`
	Scope         json.RawMessage `json:"scope,omitempty"`
}

type ConfigureResult struct {
	Granted []string `json:"granted"`
	Account string   `json:"account,omitempty"`
}

type ListParams struct {
	Scope        json.RawMessage `json:"scope,omitempty"`
	WithFullData bool            `json:"withFullData,omitempty"`
}

type GetParams struct {
	ID           string `json:"id"`
	WithFullData bool   `json:"withFullData,omitempty"`
}

type CreateParams struct {
	Scope        json.RawMessage `json:"scope,omitempty"`
	Spec         json.RawMessage `json:"spec"`
	WithFullData bool            `json:"withFullData,omitempty"`
}

type DeleteParams struct {
	ID string `json:"id"`
}

type ScopesResult struct {
	Scopes []string `json:"scopes"`
}

type TypesResult struct {
	Types []core.TypeInfo `json:"types"`
}

type AuditResult struct {
	Entries []core.AuditEntry `json:"entries"`
}
