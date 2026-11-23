package runner

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"unicode/utf8"

	"github.com/airohq/kagent-remote-harness/runner/wire"
)

// Emit sends one event to the harness.
type Emit func(wire.Event)

// messages keeps the coding agent's messages apart in its text. kagent joins
// text events into one artifact until a tool event, so the text of a message
// that follows another message, with no tool between, starts with a blank
// line. Without it "…what I'll do." and "The answer…" run together.
type messages struct {
	emit Emit
	// last is the message of the last text event, and empty after any other
	// event.
	last string
}

// text emits a piece of the message's text.
func (m *messages) text(message, text string) {
	if text == "" {
		return
	}
	if m.last != "" && m.last != message {
		text = "\n\n" + text
	}
	m.last = message
	m.emit(wire.Event{Type: wire.EventText, Text: text})
}

// event emits an event that is not text, which ends the text run.
func (m *messages) event(ev wire.Event) {
	m.last = ""
	m.emit(ev)
}

// newEmitter writes one event per line and flushes it, so the harness sees
// the coding agent's progress as it happens. It adds the machine to session
// events and cuts an event over wire.MaxEventBytes short.
func newEmitter(w http.ResponseWriter, machine string, logger *slog.Logger, logContent bool) Emit {
	flusher, _ := w.(http.Flusher)
	return func(ev wire.Event) {
		if ev.Type == wire.EventSession {
			ev.Machine = machine
		}
		line, err := EncodeEvent(ev)
		if err != nil {
			logger.Warn("encode event", "type", ev.Type, "error", err)
			return
		}
		if logContent && ev.Type == wire.EventText {
			logger.Info("text", "text", ev.Text)
		}
		_, _ = w.Write(append(line, '\n'))
		if flusher != nil {
			flusher.Flush()
		}
	}
}

const cutNotice = "… (cut short: over 1 MiB)"

// cutShort keeps an event's fields that fit in wire.MaxEventBytes.
func cutShort(ev wire.Event) wire.Event {
	limit := wire.MaxEventBytes / 4
	ev.Text = truncate(ev.Text, limit)
	ev.Message = truncate(ev.Message, limit)
	if ev.Arguments != nil {
		if b, _ := json.Marshal(ev.Arguments); len(b) > limit {
			ev.Arguments = map[string]any{"cut_short": truncate(string(b), limit)}
		}
	}
	if ev.Result != nil {
		if b, _ := json.Marshal(ev.Result); len(b) > limit {
			if s, ok := ev.Result.(string); ok {
				ev.Result = truncate(s, limit)
			} else {
				ev.Result = truncate(string(b), limit)
			}
		}
	}
	return ev
}

// truncate cuts s to at most n bytes, on a rune boundary, and says so.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len(cutNotice)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + cutNotice
}

// EncodeEvent encodes one bounded event for either HTTP or the worker stream.
func EncodeEvent(ev wire.Event) ([]byte, error) {
	line, err := json.Marshal(ev)
	if err == nil && len(line) > wire.MaxEventBytes {
		return json.Marshal(cutShort(ev))
	}
	return line, err
}
