// Package core owns flag precedence, backend selection, translation and the
// dropped-field diff. It takes a request and returns a response or an event
// sequence, and never touches os.Stdout: a core that cannot tell what invoked
// it is testable without a terminal, and the listener that comes later is an
// addition rather than a refactor.
package core

import (
	"errors"
	"fmt"

	"github.com/mdijkstra-oss/dragoman/internal/base"
)

// Options is what the caller settled before the core runs. Credentials are
// absent by construction: they come from the environment only, because argv is
// visible in ps, in shell history, and in CI logs.
type Options struct {
	Protocol        string
	BackendProtocol string
	Endpoint        string
	Preset          string

	Model     string
	Effort    string
	MaxTokens *int

	Stream *bool

	Strict    bool
	NoExecute bool
	Raw       bool
}

// ErrMissingCredential marks the one usage failure a caller did nothing to
// cause. Every other usage failure is a bad request; this one is an instance
// that was started without the key it needs, and a transport has to be able to
// tell them apart without asking the environment a second time.
var ErrMissingCredential = errors.New("missing credential")

// ExitCode separates "the backend said no" from "dragoman could not run". Two
// is reserved for the latter, which is the only thing that makes it
// trustworthy in a hook or a CI step.
type ExitCode int

const (
	ExitOK      ExitCode = 0
	ExitBackend ExitCode = 1
	ExitUsage   ExitCode = 2
)

type Failure struct {
	Code ExitCode
	Err  error

	// Status is the status line the backend refused with, kept as a number
	// rather than only as prose in the message. Rate limits and quota errors
	// carry retry semantics a client already implements, and a transport that
	// had to parse them back out of an error string would lose them.
	Status int
}

func Fail(code ExitCode, format string, args ...any) Failure {
	return Failure{Code: code, Err: fmt.Errorf(format, args...)}
}

// FailBackend records a refusal in the backend's own words and its own status.
func FailBackend(status int, format string, args ...any) Failure {
	return Failure{Code: ExitBackend, Err: fmt.Errorf(format, args...), Status: status}
}

func (f Failure) Error() string { return f.Err.Error() }

func (f Failure) Unwrap() error { return f.Err }

// CodeOf reports the exit code an error should produce. Anything that is not a
// Failure could not have been anticipated, so it is a usage failure.
func CodeOf(err error) ExitCode {
	if err == nil {
		return ExitOK
	}
	var failure Failure
	if errors.As(err, &failure) {
		return failure.Code
	}
	return ExitUsage
}

// StatusOf reports the status a backend refused with, or zero when the failure
// did not come from a backend refusing.
func StatusOf(err error) int {
	var failure Failure
	if errors.As(err, &failure) {
		return failure.Status
	}
	return 0
}

// Resolve applies flag precedence to a request: the flag, then the value in
// the body, then nothing. Absent must stay absent, so a level nobody asked for
// is never filled in here.
func Resolve(req base.Request, opts Options) base.Request {
	if opts.Model != "" {
		model := opts.Model
		req.Model = &model
	}
	if opts.Effort != "" {
		effort := opts.Effort
		if req.Reasoning == nil {
			req.Reasoning = &base.Reasoning{}
		}
		reasoning := *req.Reasoning
		reasoning.Effort = &effort
		req.Reasoning = &reasoning
	}
	if opts.MaxTokens != nil {
		cap := *opts.MaxTokens
		req.MaxOutputTokens = &cap
	}
	return req
}

// WantsEvents reports what the caller receives. stream is an output setting:
// it decides what the caller gets, not how dragoman talks to the backend.
func WantsEvents(req base.Request, opts Options) bool {
	if opts.Stream != nil {
		return *opts.Stream
	}
	return base.GetStream(req)
}
