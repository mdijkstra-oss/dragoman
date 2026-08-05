package serve

import (
	"errors"
	"net/http"

	"github.com/mdijkstra-oss/dragoman/internal/base"
)

// The Responses format specifies an event line naming the type, the payload, and
// a blank line closing it.
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

func (s *stream) Started() bool { return s.started }

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

// The event line repeats the payload's own type field. An SSE client dispatches
// on the name before parsing anything; the field survives a client that ignores
// framing.
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
