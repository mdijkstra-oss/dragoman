package base

import "fmt"

// Frame is one unit of a backend's response stream: an SSE event name (empty
// where the dialect omits it) and its payload. Framing belongs to the
// transport, so every dialect decoder consumes this one shape.
type Frame struct {
	Event string
	Data  []byte
}

// StreamDecoder is the erased form of a per-dialect transducer, so that
// dialect selection is a dispatch table rather than a type switch (R-4).
type StreamDecoder struct {
	Step  func(Frame) ([]Event, error)
	Flush func() ([]Event, error)
}

// Transduce erases a typed, pure transducer into the uniform form the dispatch
// table holds. Step and Flush are functions from data to data with no IO and
// never mutate the state they are handed; this thin wrapper is the only place
// the state is threaded for them.
func Transduce[S any](
	init S,
	step func(S, Frame) (S, []Event, error),
	flush func(S) ([]Event, error),
) StreamDecoder {
	state := init
	return StreamDecoder{
		Step: func(frame Frame) ([]Event, error) {
			next, events, err := step(state, frame)
			if err != nil {
				return nil, err
			}
			state = next
			return events, nil
		},
		Flush: func() ([]Event, error) { return flush(state) },
	}
}

// Fold reduces an event sequence to the response it describes. Folding a
// translated event stream must equal the translated buffered response, for
// every fixture and every backend; both sides are the same emitter snapshot,
// which is what makes that invariant hold by construction rather than by
// agreement.
func Fold(events []Event) (Response, error) {
	for _, event := range events {
		if IsTerminal(event) && event.Response != nil {
			return *event.Response, nil
		}
	}
	return Response{}, fmt.Errorf("fold: event sequence has no terminal response")
}
