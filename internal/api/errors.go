package api

import (
	"errors"
	"fmt"
	"net/http"
)

// Error codes. They are part of the public contract: scripts and agents can
// branch on them.
const (
	CodeInvalid          = "invalid_request"
	CodeNotFound         = "not_found"
	CodeConflict         = "conflict"
	CodeDeployInProgress = "deploy_in_progress"
	CodeBuildFailed      = "build_failed"
	CodeReleaseFailed    = "release_failed"
	CodeUnhealthy        = "health_check_failed"
	CodeDeployFailed     = "deploy_failed"
	CodeRuntime          = "runtime_error"
	CodeUnreachable      = "server_unreachable"
	CodeNoContext        = "no_server_configured"
	CodeConfirmation     = "confirmation_required"
	CodeInternal         = "internal_error"
	CodeCommandFailed    = "command_failed"
)

// Exit codes used by the CLI.
const (
	ExitOK          = 0
	ExitError       = 1
	ExitUsage       = 2
	ExitNotFound    = 3
	ExitConflict    = 4
	ExitDeployFail  = 5
	ExitUnreachable = 6
)

// Error is the structured error returned by the API and printed by the CLI.
type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Hint    string         `json:"hint,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string { return e.Message }

func Errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func (e *Error) WithHint(format string, args ...any) *Error {
	e.Hint = fmt.Sprintf(format, args...)
	return e
}

func (e *Error) WithDetail(key string, value any) *Error {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	e.Details[key] = value
	return e
}

// AsError converts any error into a structured *Error.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: CodeInternal, Message: err.Error()}
}

// HTTPStatus maps an error code to an HTTP status.
func (e *Error) HTTPStatus() int {
	switch e.Code {
	case CodeInvalid, CodeConfirmation:
		return http.StatusBadRequest
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict, CodeDeployInProgress:
		return http.StatusConflict
	case CodeBuildFailed, CodeReleaseFailed, CodeUnhealthy, CodeDeployFailed, CodeCommandFailed:
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}

// ExitCode maps an error code to a process exit code.
func (e *Error) ExitCode() int {
	switch e.Code {
	case CodeInvalid, CodeConfirmation:
		return ExitUsage
	case CodeNotFound:
		return ExitNotFound
	case CodeConflict, CodeDeployInProgress:
		return ExitConflict
	case CodeBuildFailed, CodeReleaseFailed, CodeUnhealthy, CodeDeployFailed:
		return ExitDeployFail
	case CodeUnreachable, CodeNoContext:
		return ExitUnreachable
	default:
		return ExitError
	}
}

type ErrorResponse struct {
	Error *Error `json:"error"`
}
