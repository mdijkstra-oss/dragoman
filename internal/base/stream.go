package base

import "fmt"

type Frame struct {
	Event string
	Data  []byte
}

type StreamDecoder struct {
	Step  func(Frame) ([]Event, error)
	Flush func() ([]Event, error)
}

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

func Fold(events []Event) (Response, error) {
	for _, event := range events {
		if IsTerminal(event) && event.Response != nil {
			return *event.Response, nil
		}
	}
	return Response{}, fmt.Errorf("fold: event sequence has no terminal response")
}
