package serve

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mdijkstra-oss/dragoman/internal/base"
)

// TestFailIsSuppressedOnlyForAnAlreadyFailedSequence covers the difference
// between a failure the caller has already been told about and one it has not.
// Suppressing every terminal event hands a caller a well-formed answer dragoman
// had already decided not to stand behind: a drop caught under strict is found
// after the last event, so the sequence ends on response.completed and the
// refusal arrives afterwards.
func TestFailIsSuppressedOnlyForAnAlreadyFailedSequence(t *testing.T) {
	cases := []struct {
		name    string
		written []base.EventType
		want    bool
	}{
		{name: "clean completion still carries a later failure", written: []base.EventType{base.EventCreated, base.EventCompleted}, want: true},
		{name: "incomplete still carries a later failure", written: []base.EventType{base.EventCreated, base.EventIncomplete}, want: true},
		{name: "mid-sequence failure is reported", written: []base.EventType{base.EventCreated, base.EventTextDelta}, want: true},
		{name: "an already failed sequence is left alone", written: []base.EventType{base.EventCreated, base.EventFailed}, want: false},
		{name: "nothing written is a status, not an event", written: nil, want: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			events := newStream(recorder)
			for index, eventType := range testCase.written {
				send(t, events, eventType, index+1)
			}

			if err := events.Fail(base.ResponseError{Message: "strict: 1 field(s) dropped"}); err != nil {
				t.Fatalf("fail: %v", err)
			}

			reported := strings.Contains(recorder.Body.String(), "strict: 1 field(s) dropped")
			if reported != testCase.want {
				t.Errorf("failure reported = %v, want %v\n%s", reported, testCase.want, recorder.Body.String())
			}
		})
	}
}

// TestFailContinuesTheSequence checks the synthesized event is one the caller's
// parser can still read: the sequence number keeps counting and the frame is
// shaped like every other.
func TestFailContinuesTheSequence(t *testing.T) {
	recorder := httptest.NewRecorder()
	events := newStream(recorder)
	send(t, events, base.EventCreated, 1)
	send(t, events, base.EventCompleted, 7)

	if err := events.Fail(base.ResponseError{Message: "refused"}); err != nil {
		t.Fatalf("fail: %v", err)
	}

	lines := dataLines(recorder.Body.String())
	if len(lines) != 3 {
		t.Fatalf("events = %d, want the two written plus the failure", len(lines))
	}
	last := lines[2]
	if !strings.Contains(last, `"sequence_number":8`) {
		t.Errorf("failure did not continue the sequence: %s", last)
	}
	if !strings.Contains(last, string(base.EventFailed)) {
		t.Errorf("failure is not a %s event: %s", base.EventFailed, last)
	}
	if !strings.HasSuffix(recorder.Body.String(), "\n\n") {
		t.Error("failure event is not terminated by a blank line")
	}
}

func send(t *testing.T, events *stream, eventType base.EventType, sequence int) {
	t.Helper()
	event := base.Event{Type: eventType, SequenceNumber: sequence}
	if base.IsTerminal(event) || eventType == base.EventCreated {
		event.Response = &base.Response{Object: base.ObjectResponse, ID: "resp_1"}
	}
	if err := events.Send(event); err != nil {
		t.Fatalf("send %s: %v", eventType, err)
	}
}

func dataLines(body string) []string {
	lines := []string{}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, dataPrefix) {
			lines = append(lines, strings.TrimPrefix(line, dataPrefix))
		}
	}
	return lines
}
