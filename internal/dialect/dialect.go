package dialect

import (
	"encoding/json"

	"github.com/mdijkstra-oss/dragoman/internal/base"
)

type Dialect struct {
	Name string

	Path string

	Headers func(key string) map[string]string

	KeyEnv string

	Encode func(req base.Request, backend base.Backend, stream bool) (Encoded, error)

	// StreamPieces recognises what happened in one wire frame.
	StreamPieces func(state State, frame base.Frame) (State, Recognised, error)

	// BodyPieces recognises the same things in a whole buffered body. The
	// per-dialect difference between the two paths is only how pieces are
	// extracted; every translation decision lives in one place.
	BodyPieces func(body []byte) (Recognised, error)

	NewState func() State

	DropTable map[string]base.DropReason

	StreamUpstream bool
}

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
