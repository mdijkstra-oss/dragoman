package core

import (
	"errors"
	"fmt"

	"github.com/mdijkstra-oss/dragoman/internal/base"
)

// No credential appears here: argv is visible in ps, in shell history, and in
// CI logs.
type Options struct {
	Protocol        string
	BackendProtocol string
	Endpoint        string
	Preset          string

	// The name of the variable holding the key, for a wire format several
	// companies serve and no default credential fits.
	AuthEnv string

	Model     string
	Effort    string
	MaxTokens *int

	Stream *bool

	Strict    bool
	NoExecute bool
	Raw       bool
}

var ErrMissingCredential = errors.New("missing credential")

type ExitCode int

const (
	ExitOK      ExitCode = 0
	ExitBackend ExitCode = 1
	ExitUsage   ExitCode = 2
)

type Failure struct {
	Code ExitCode
	Err  error

	// Rate limits and quota errors carry retry semantics clients already
	// implement, which parsing them back out of a message would lose.
	Status int
}

func Fail(code ExitCode, format string, args ...any) Failure {
	return Failure{Code: code, Err: fmt.Errorf(format, args...)}
}

func FailBackend(status int, format string, args ...any) Failure {
	return Failure{Code: ExitBackend, Err: fmt.Errorf(format, args...), Status: status}
}

func (f Failure) Error() string { return f.Err.Error() }

func (f Failure) Unwrap() error { return f.Err }

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

func StatusOf(err error) int {
	var failure Failure
	if errors.As(err, &failure) {
		return failure.Status
	}
	return 0
}

// Flag, then body, then nothing. Filling in a level nobody asked for overrides
// a provider default and changes cost and behaviour.
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

func WantsEvents(req base.Request, opts Options) bool {
	if opts.Stream != nil {
		return *opts.Stream
	}
	return base.GetStream(req)
}
