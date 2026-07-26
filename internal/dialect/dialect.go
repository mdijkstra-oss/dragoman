// Package dialect holds the shape every wire format implements. A dialect is a
// wire shape, not a vendor: openai-completions names a body format that several
// companies serve. No dialect package may import another — if two ever
// reference each other the design has become N×M.
package dialect

import (
	"encoding/json"

	"github.com/matthijn/dragoman/internal/base"
)

// Dialect is everything dragoman needs to talk one wire format. Translation
// runs in exactly one direction per side: Encode builds a request out of the
// base toward a backend, and the extractors decode a response into the base.
type Dialect struct {
	Name string

	// Path is appended to a backend's endpoint to reach the operation.
	Path string

	// Headers builds the request headers, including authentication. The key
	// comes from the environment, never from argv.
	Headers func(key string) map[string]string

	// KeyEnv names the environment variable holding the credential.
	KeyEnv string

	// Encode builds the backend body out of the base request.
	Encode func(req base.Request, backend base.Backend, stream bool) (Encoded, error)

	// StreamPieces recognises what happened in one wire frame.
	StreamPieces func(state State, frame base.Frame) (State, Recognised, error)

	// BodyPieces recognises the same things in a whole buffered body. The
	// per-dialect difference between the two paths is only how pieces are
	// extracted; every translation decision lives in one place.
	BodyPieces func(body []byte) (Recognised, error)

	// NewState builds the dialect's own parsing state.
	NewState func() State

	// DropTable classifies the base fields this dialect can never express.
	// Anything it does not name is unimplemented, so the backlog stays
	// visible inside the list of permanent impossibilities.
	DropTable map[string]base.DropReason
}

// State is a dialect's own parsing state. It is threaded through StreamPieces
// and never mutated; each dialect defines its own concrete type.
type State any

type Encoded struct {
	Body  json.RawMessage
	Drops []base.Drop
}

// Recognised is what a decoder made of one frame or one body: the pieces it
// understood, and the fields it did not. Each dialect computes its own drops
// with base.Diff against the typed value that consumed the bytes, so the list
// stays computed rather than hand-maintained.
type Recognised struct {
	Pieces []base.Piece
	Drops  []base.Drop
}

// Decoder builds the erased stream decoder for a dialect, wiring its piece
// extractor to the one emitter that mints Responses bookkeeping. Drops seen
// mid-stream are collected through the supplied callback, because the
// transport decides where they go.
func Decoder(d Dialect, onDrops func([]base.Drop)) base.StreamDecoder {
	type machine struct {
		emitter base.Emitter
		dialect State
	}
	init := machine{dialect: d.NewState()}
	step := func(m machine, frame base.Frame) (machine, []base.Event, error) {
		next, recognised, err := d.StreamPieces(m.dialect, frame)
		if err != nil {
			return m, nil, err
		}
		m.dialect = next
		if len(recognised.Drops) > 0 && onDrops != nil {
			onDrops(recognised.Drops)
		}
		emitter, events := base.ApplyAll(m.emitter, recognised.Pieces)
		m.emitter = emitter
		return m, events, nil
	}
	flush := func(m machine) ([]base.Event, error) {
		_, events := base.Finish(m.emitter)
		return events, nil
	}
	return base.Transduce(init, step, flush)
}

// DecodeBody folds a buffered body through the same emitter the stream drives,
// so folding a translated event stream equals the translated buffered
// response for every fixture and every backend.
func DecodeBody(d Dialect, body []byte) ([]base.Event, []base.Drop, error) {
	recognised, err := d.BodyPieces(body)
	if err != nil {
		return nil, nil, err
	}
	emitter, events := base.ApplyAll(base.Emitter{}, recognised.Pieces)
	_, finished := base.Finish(emitter)
	return append(events, finished...), recognised.Drops, nil
}

func Endpoint(d Dialect, backend base.Backend) string {
	return trimSlash(backend.Endpoint) + d.Path
}

func trimSlash(url string) string {
	for len(url) > 0 && url[len(url)-1] == '/' {
		url = url[:len(url)-1]
	}
	return url
}
