// Package serve is the HTTP transport. It owns what a listener adds and
// nothing more: routing a service name out of the path, framing the answer as
// one JSON body or as server-sent events, and mapping a failure to a status.
// Flag precedence, backend selection, translation and the dropped-field diff
// are core concerns it hands over and never implements, which is what makes
// this a sibling of the pipe rather than a second pipeline.
package serve

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/matthijn/dragoman/internal/base"
	"github.com/matthijn/dragoman/internal/core"
)

// Config is what the operator settled before the listener runs. There is no
// model, effort or cap here on purpose: those travel in the body, and a
// server-side default would make the same request mean different things at
// different instances.
type Config struct {
	Catalog  core.Catalog
	Executor core.Executor
	Strict   bool

	// Log is where drops go. A nil logger discards them; it never turns them
	// into part of the answer.
	Log *slog.Logger
}

// Service is one row of GET /services. It names the variable a service needs
// and never a key, and never whether one is set: an instance describes what it
// can address, not what it holds.
type Service struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Endpoint string `json:"endpoint"`
	Auth     string `json:"auth"`
}

// StatusBackendRefused answers a refusal the backend did not attach a status
// to — a provider error arriving in the body with a 200 on the status line.
// When there is a real status it is passed through instead, so that a rate
// limit keeps the retry semantics an SDK already implements.
const StatusBackendRefused = http.StatusBadGateway

// New builds the listener. It holds no state beyond these dependencies, so two
// instances serving the same table answer the same request the same way.
func New(cfg Config) http.Handler {
	listener := server{
		catalog:  cfg.Catalog,
		executor: cfg.Executor,
		strict:   cfg.Strict,
		log:      logger(cfg.Log),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/responses/{service}", listener.only(http.MethodPost, listener.translate))
	mux.HandleFunc("/services", listener.only(http.MethodGet, listener.services))
	mux.HandleFunc("/health", listener.only(http.MethodGet, health))
	mux.HandleFunc("/", listener.unrouted)
	return mux
}

type server struct {
	catalog  core.Catalog
	executor core.Executor
	strict   bool
	log      *slog.Logger
}

func (s server) translate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("service")
	backend, served := s.catalog.Presets[name]
	if !served {
		s.fail(w, r, http.StatusNotFound, fmt.Sprintf("unknown service %q; this instance serves: %s",
			name, strings.Join(serviceNames(s.catalog.Presets), ", ")))
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, fmt.Sprintf("reading request body: %s", err))
		return
	}
	req, err := base.ParseRequest(body)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if !hasCredential(backend) {
		s.fail(w, r, http.StatusInternalServerError,
			fmt.Sprintf("service %q is unavailable: %s is not set", name, backend.KeyEnv))
		return
	}

	// The body already carries model, effort, cap and stream, so the path
	// segment plus the server-wide strict flag is the whole of what the
	// listener settles.
	opts := core.Options{Preset: name, Strict: s.strict}
	clearWriteDeadline(w)
	if core.WantsEvents(req, opts) {
		s.streamed(w, r, opts, body)
		return
	}
	s.buffered(w, r, opts, body)
}

func (s server) buffered(w http.ResponseWriter, r *http.Request, opts core.Options, body []byte) {
	result, err := core.Run(r.Context(), s.catalog, opts, body, s.executor, nil)
	s.record(opts.Preset, result.Drops)
	if err != nil {
		s.fail(w, r, statusOf(err), err.Error())
		return
	}
	encoded, err := json.Marshal(result.Response)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, fmt.Sprintf("encoding response: %s", err))
		return
	}
	writeJSON(w, http.StatusOK, encoded)
}

func (s server) streamed(w http.ResponseWriter, r *http.Request, opts core.Options, body []byte) {
	events := newStream(w)
	result, err := core.Run(r.Context(), s.catalog, opts, body, s.executor, events.Send)
	s.record(opts.Preset, result.Drops)
	if err == nil {
		return
	}
	// The status line went out with the first event, so what is left to say has
	// to be said in the format the caller is already reading.
	if events.Started() {
		s.log.Error("stream failed", "service", opts.Preset, "error", err.Error())
		_ = events.Fail(responseError(statusOf(err), err.Error()))
		return
	}
	s.fail(w, r, statusOf(err), err.Error())
}

func (s server) services(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, encodeStrings(getServices(s.catalog.Presets)))
}

func (s server) unrouted(w http.ResponseWriter, r *http.Request) {
	s.fail(w, r, http.StatusNotFound, fmt.Sprintf("no route for %s; this instance serves "+
		"POST /responses/<service>, GET /services and GET /health", r.URL.Path))
}

func health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []byte(liveness))
}

// only guards a route to one method. The listener answers a wrong method in
// the same error shape as everything else, because a client that can only read
// one format has to be able to read this too.
func (s server) only(method string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			s.fail(w, r, http.StatusMethodNotAllowed,
				fmt.Sprintf("%s is not allowed on %s; use %s", r.Method, r.URL.Path, method))
			return
		}
		next(w, r)
	}
}

// record puts drops in the server log and nowhere else. The response is the
// answer; a caller who wants the diff has the pipe.
func (s server) record(service string, drops []base.Drop) {
	if len(drops) == 0 {
		return
	}
	s.log.Warn("dropped fields", "service", service, "drops", drops)
}

func (s server) fail(w http.ResponseWriter, r *http.Request, status int, message string) {
	s.log.Log(r.Context(), levelFor(status), "request failed",
		"method", r.Method, "path", r.URL.Path, "status", status, "error", message)
	writeFailure(w, status, message)
}

// statusOf maps a run failure to a status. The unknown service and the unset
// credential never reach it: the core reports both as the same usage failure an
// unparseable body produces, so the listener decides those before it runs.
func statusOf(err error) int {
	switch code := core.CodeOf(err); code {
	case core.ExitOK:
		return http.StatusOK
	case core.ExitBackend:
		// Only a status a client can act on is worth passing through. Anything
		// outside the error range carries no retry semantics, and handing an
		// out-of-range number to WriteHeader panics the handler.
		if status := core.StatusOf(err); status >= 400 && status <= 599 {
			return status
		}
		return StatusBackendRefused
	case core.ExitUsage:
		return http.StatusBadRequest
	default:
		panic(fmt.Sprintf("serve: unknown exit code %d", code))
	}
}

// hasCredential asks the question core.Run asks again before it calls a
// backend. It is asked twice because the answer decides a status and the core
// cannot say which of its usage failures this was; a distinguishable failure in
// the core is the fix, not a check the transport keeps.
func hasCredential(backend base.Backend) bool { return os.Getenv(backend.KeyEnv) != "" }

// getServices renders the table as data, sorted so an instance describes itself
// the same way twice.
func getServices(table map[string]base.Backend) []Service {
	rows := make([]Service, 0, len(table))
	for _, name := range serviceNames(table) {
		backend := table[name]
		rows = append(rows, Service{
			Name:     name,
			Protocol: backend.Dialect,
			Endpoint: backend.Endpoint,
			Auth:     backend.KeyEnv,
		})
	}
	return rows
}

func serviceNames(table map[string]base.Backend) []string {
	return slices.Sorted(maps.Keys(table))
}

// failureBody is an openai-responses error object.
type failureBody struct {
	Error base.ResponseError `json:"error"`
}

func writeFailure(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, encodeStrings(failureBody{Error: responseError(status, message)}))
}

func responseError(status int, message string) base.ResponseError {
	return base.ResponseError{Type: errorType(status), Message: message}
}

// errorType names the failure in the caller's vocabulary: a 5xx is dragoman's
// or the backend's, and everything else is the request's.
func errorType(status int) string {
	if status >= http.StatusInternalServerError {
		return "server_error"
	}
	return "invalid_request_error"
}

func writeJSON(w http.ResponseWriter, status int, encoded []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

// encodeStrings renders a value whose fields are all strings. Such a value
// cannot fail to encode, so a failure here is a programmer error.
func encodeStrings(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic("serve: " + err.Error())
	}
	return encoded
}

// clearWriteDeadline keeps a long answer from being truncated by a deadline the
// server set for ordinary requests. Not every writer carries one.
func clearWriteDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
}

func levelFor(status int) slog.Level {
	if status >= http.StatusInternalServerError {
		return slog.LevelError
	}
	return slog.LevelWarn
}

func logger(supplied *slog.Logger) *slog.Logger {
	if supplied != nil {
		return supplied
	}
	return slog.New(slog.DiscardHandler)
}

const liveness = `{"status":"ok"}`
