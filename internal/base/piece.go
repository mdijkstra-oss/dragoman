package base

type PieceKind string

const (
	PieceStart   PieceKind = "start"
	PieceOpen    PieceKind = "open"
	PieceText    PieceKind = "text"
	PieceArgs    PieceKind = "args"
	PieceSummary PieceKind = "summary"
	PieceCall    PieceKind = "call"
	PieceState   PieceKind = "state"
	PieceClose   PieceKind = "close"
	PieceUsage   PieceKind = "usage"
	PieceOutcome PieceKind = "outcome"
)

type Piece struct {
	Kind PieceKind

	Key      string
	ItemKind ItemType

	Text   string
	Name   string
	CallID string
	State  string

	ID        string
	Model     string
	CreatedAt int64

	Usage      *Usage
	Status     string
	Incomplete *IncompleteDetails
	Error      *ResponseError
}

func Start(id, model string, createdAt int64) Piece {
	return Piece{Kind: PieceStart, ID: id, Model: model, CreatedAt: createdAt}
}

func OpenPiece(key string, kind ItemType) Piece {
	return Piece{Kind: PieceOpen, Key: key, ItemKind: kind}
}

func OpenIdentified(key string, kind ItemType, itemID string) Piece {
	return Piece{Kind: PieceOpen, Key: key, ItemKind: kind, ID: itemID}
}

func TextPiece(key, text string) Piece { return Piece{Kind: PieceText, Key: key, Text: text} }

func ArgsPiece(key, fragment string) Piece {
	return Piece{Kind: PieceArgs, Key: key, Text: fragment}
}

func SummaryPiece(key, text string) Piece { return Piece{Kind: PieceSummary, Key: key, Text: text} }

func CallPiece(key, callID, name string) Piece {
	return Piece{Kind: PieceCall, Key: key, CallID: callID, Name: name}
}

func StatePiece(key, state string) Piece { return Piece{Kind: PieceState, Key: key, State: state} }

func ClosePiece(key string) Piece { return Piece{Kind: PieceClose, Key: key} }

func UsagePiece(usage *Usage) Piece { return Piece{Kind: PieceUsage, Usage: usage} }

func FailurePiece(code, message string) Piece {
	return OutcomePiece(StatusFailed, nil, &ResponseError{Code: code, Message: message})
}

func OutcomePiece(status string, incomplete *IncompleteDetails, err *ResponseError) Piece {
	return Piece{Kind: PieceOutcome, Status: status, Incomplete: incomplete, Error: err}
}

func Apply(e Emitter, piece Piece) (Emitter, []Event) {
	handler, known := pieceHandlers[piece.Kind]
	if !known {
		panic("base: unknown piece kind " + string(piece.Kind))
	}
	return handler(e, piece)
}

func ApplyAll(e Emitter, pieces []Piece) (Emitter, []Event) {
	events := []Event{}
	for _, piece := range pieces {
		var produced []Event
		e, produced = Apply(e, piece)
		events = append(events, produced...)
	}
	return e, events
}

var pieceHandlers = map[PieceKind]func(Emitter, Piece) (Emitter, []Event){
	PieceStart: func(e Emitter, p Piece) (Emitter, []Event) {
		e.ResponseID, e.Model, e.CreatedAt = p.ID, p.Model, p.CreatedAt
		return Begin(e)
	},
	PieceOpen: func(e Emitter, p Piece) (Emitter, []Event) {
		return OpenAs(e, p.Key, p.ItemKind, p.ID)
	},
	PieceText: func(e Emitter, p Piece) (Emitter, []Event) {
		return TextDelta(e, p.Key, p.Text)
	},
	PieceArgs: func(e Emitter, p Piece) (Emitter, []Event) {
		return ArgumentsDelta(e, p.Key, p.Text)
	},
	PieceSummary: func(e Emitter, p Piece) (Emitter, []Event) {
		return SummaryDelta(e, p.Key, p.Text)
	},
	PieceCall: func(e Emitter, p Piece) (Emitter, []Event) {
		return SetCall(e, p.Key, p.CallID, p.Name)
	},
	PieceState: func(e Emitter, p Piece) (Emitter, []Event) {
		return SetState(e, p.Key, p.State)
	},
	PieceClose: func(e Emitter, p Piece) (Emitter, []Event) {
		return Close(e, p.Key)
	},
	PieceUsage: func(e Emitter, p Piece) (Emitter, []Event) {
		return SetUsage(e, p.Usage), nil
	},
	PieceOutcome: func(e Emitter, p Piece) (Emitter, []Event) {
		return SetOutcome(e, p.Status, p.Incomplete, p.Error), nil
	},
}
