package snow

import (
	"fmt"

	"github.com/tcurtsinger/GlideMind/internal/exit"
)

// APIError is a non-2xx response from the instance, carrying the ServiceNow
// error envelope when one was present.
type APIError struct {
	StatusCode int
	Message    string
	Detail     string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = "request failed"
	}
	if e.Detail != "" && e.Detail != e.Message {
		msg += ": " + e.Detail
	}
	return fmt.Sprintf("%s (HTTP %d)", msg, e.StatusCode)
}

// ExitCode maps HTTP status onto glm's exit-code contract.
func (e *APIError) ExitCode() int {
	switch e.StatusCode {
	case 401, 403:
		return exit.Auth
	case 404:
		return exit.NotFound
	default:
		return exit.API
	}
}

// NetworkError wraps transport-level failures (DNS, TLS, timeouts).
type NetworkError struct{ Err error }

func (e *NetworkError) Error() string { return "network: " + e.Err.Error() }
func (e *NetworkError) Unwrap() error { return e.Err }
func (e *NetworkError) ExitCode() int { return exit.Network }
