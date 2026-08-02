package serve

import (
	"errors"
	"net/http"

	"github.com/mdijkstra-oss/dragoman/internal/base"
)

// stream frames the event sequence as server-sent events. The payload is the
// line the pipe writes as NDJSON, introduced by an event line naming its type
// and closed by a blank line, which is the framing the Responses format
// specifies. A client that dispatches on the event name reads the same stream
// as one that dispatches on the type inside the payload.
type stream struct {
	writer  http.ResponseWriter
	control *http.ResponseController

	started  bool
	last     base.EventType
	sequence int
	opened   *base.Response
}

func newStream(w http.ResponseWriter) *stream {
	return &stream{writer: w, control: http.NewResponseController(w)}
}

// Send writes one event and pushes it out. Events are flushed one at a time:
// accumulating them yields byte-identical output while defeating the whole
// purpose of the mode, so this is a correctness requirement, not a performance
// note.
func (s *stream) Send(event base.Event) error {
	// The payload is compact JSON, so every newline inside it is escaped and an
	// event is always exactly one data line.
	encoded, err := base.EncodeEvent(event)
	if err != nil {
		return err
	}
	s.begin()
	if _, err := s.writer.Write(frame(event.Type, encoded)); err != nil {
		return err
	}
	s.remember(event)
	return s.flush()
}

// Started reports whether the status line has already gone out. That is what
// makes a mid-stream failure an event rather than a status, and it stays false
// until the first event so that a failure before one is still a status.
func (s *stream) Started() bool { return s.started }

// Fail ends a stream that has already begun. Only a sequence that already ended
// in a failure is left alone, because that failure is the one the core read out
// of the answer and saying it twice would corrupt the sequence. A sequence that
// ended cleanly still has to carry a later failure — a drop caught under strict
// is found after the last event, and swallowing it would hand the caller a
// well-formed answer dragoman had already decided not to stand behind.
func (s *stream) Fail(cause base.ResponseError) error {
	if !s.started || s.last == base.EventFailed {
		return nil
	}
	snapshot := base.Response{Object: base.ObjectResponse}
	if s.opened != nil {
		snapshot = *s.opened
	}
	snapshot.Status = base.StatusFailed
	snapshot.Error = &cause
	return s.Send(base.Event{
		Type:           base.EventFailed,
		SequenceNumber: s.sequence + 1,
		Response:       &snapshot,
	})
}

func (s *stream) begin() {
	if s.started {
		return
	}
	s.started = true
	header := s.writer.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	s.writer.WriteHeader(http.StatusOK)
}

// remember keeps what a failure event would have to carry: where the sequence
// got to, how it ended, and the response the lifecycle was opened with.
func (s *stream) remember(event base.Event) {
	s.sequence = event.SequenceNumber
	s.last = event.Type
	if event.Response != nil {
		s.opened = event.Response
	}
}

func (s *stream) flush() error {
	if err := s.control.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}

// The event line repeats what the payload's own type field already says. Both
// are required: the name is what an SSE client dispatches on before parsing
// anything, and the field is what survives a client that ignores framing.
func frame(name base.EventType, payload []byte) []byte {
	framed := make([]byte, 0, len(eventPrefix)+len(name)+len(dataPrefix)+len(payload)+3)
	framed = append(framed, eventPrefix...)
	framed = append(framed, name...)
	framed = append(framed, '\n')
	framed = append(framed, dataPrefix...)
	framed = append(framed, payload...)
	return append(framed, '\n', '\n')
}

const (
	eventPrefix = "event: "
	dataPrefix  = "data: "
)
