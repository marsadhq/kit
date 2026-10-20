package event

import (
	"encoding/json"
	"errors"
	"time"
)

// TendEvent is the event object Tend embeds in its webhook body under
// "event". It is Tend's stored event, and its keys are the Go field names of
// Tend's original event type, capitalised: they have been on the wire since
// Tend's first release and must never change.
//
// Decoding is lenient where Ward's has always been: ID, OrgID, DedupKey and
// CreatedAt are hints, and a value of the wrong type leaves the zero value
// instead of failing the whole body. Type, Source and Payload must be strings.
type TendEvent struct {
	ID        int64     `json:"ID"`
	OrgID     int64     `json:"OrgID"`
	Type      string    `json:"Type"`      // e.g. "run.failed", "heartbeat.missed"
	Source    string    `json:"Source"`    // e.g. "jobs.runner", "heartbeat"
	Payload   string    `json:"Payload"`   // JSON for run and job events, a plain name for heartbeats
	DedupKey  string    `json:"DedupKey"`  // e.g. "run:41:run.failed"; empty for heartbeat events
	CreatedAt time.Time `json:"CreatedAt"` // when Tend stored it; zero from Tend before it sent stored events
}

// UnmarshalJSON decodes a TendEvent, leniently for the hint fields.
func (e *TendEvent) UnmarshalJSON(b []byte) error {
	var raw struct {
		ID        lenientInt64  `json:"ID"`
		OrgID     lenientInt64  `json:"OrgID"`
		Type      string        `json:"Type"`
		Source    string        `json:"Source"`
		Payload   string        `json:"Payload"`
		DedupKey  lenientString `json:"DedupKey"`
		CreatedAt lenientTime   `json:"CreatedAt"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*e = TendEvent{
		ID:        int64(raw.ID),
		OrgID:     int64(raw.OrgID),
		Type:      raw.Type,
		Source:    raw.Source,
		Payload:   raw.Payload,
		DedupKey:  string(raw.DedupKey),
		CreatedAt: raw.CreatedAt.Time,
	}
	return nil
}

// TendWebhook is the body Tend's webhook channel posts:
//
//	{"body":"...","event":{"ID":...,"OrgID":...,"Type":"...","Source":"...",
//	 "Payload":"...","DedupKey":"...","CreatedAt":"..."},"subject":"..."}
//
// plus, optionally, the V1 envelope under "marsad". A receiver that only knows
// the legacy keys keeps working, since it ignores "marsad"; one that knows the
// envelope can prefer it.
//
// The fields are declared in key order, so encoding a TendWebhook without an
// envelope produces exactly the bytes Tend's earlier map-based encoding did,
// and the envelope lands between "event" and "subject".
type TendWebhook struct {
	Body    string     `json:"body"`
	Event   *TendEvent `json:"event"`
	Marsad  *V1        `json:"marsad,omitempty"`
	Subject string     `json:"subject"`

	// envelopeErr is why a "marsad" value present in a decoded body was
	// dropped; see EnvelopeError.
	envelopeErr error
}

// ErrNotTendWebhook is returned by ParseTendWebhook for a JSON body that has
// no Tend "event" object.
var ErrNotTendWebhook = errors.New("event: not a Tend webhook body (no event object)")

// UnmarshalJSON decodes a Tend webhook body. A body that is not JSON, or
// whose subject, body, or event Type, Source or Payload is not a string, is an
// error. The envelope never fails the decode: when "marsad" is present but is
// not a valid V1 envelope, Marsad is left nil and EnvelopeError says why, so
// the receiver falls back to the legacy fields it can always rely on.
func (w *TendWebhook) UnmarshalJSON(b []byte) error {
	var raw struct {
		Body    string          `json:"body"`
		Event   *TendEvent      `json:"event"`
		Marsad  json.RawMessage `json:"marsad"`
		Subject string          `json:"subject"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*w = TendWebhook{Body: raw.Body, Event: raw.Event, Subject: raw.Subject}
	if len(raw.Marsad) > 0 && string(raw.Marsad) != "null" {
		env, err := ParseV1(raw.Marsad)
		if err != nil {
			w.envelopeErr = err
		} else {
			w.Marsad = &env
		}
	}
	return nil
}

// EnvelopeError reports why the "marsad" value of a decoded body was not
// used, or nil when there was none or it was valid.
func (w TendWebhook) EnvelopeError() error { return w.envelopeErr }

// ParseTendWebhook decodes a body Tend posted (see UnmarshalJSON). A JSON
// body without an "event" object is ErrNotTendWebhook: it came from some
// other sender.
func ParseTendWebhook(data []byte) (TendWebhook, error) {
	var w TendWebhook
	if err := json.Unmarshal(data, &w); err != nil {
		return TendWebhook{}, err
	}
	if w.Event == nil {
		return TendWebhook{}, ErrNotTendWebhook
	}
	return w, nil
}

// lenientTime decodes an optional RFC 3339 timestamp. Anything else (an
// empty string, another layout, a number, an object) leaves the zero time
// instead of failing the decode of the whole body.
type lenientTime struct{ time.Time }

// UnmarshalJSON never returns an error: an unreadable hint is "unknown".
func (t *lenientTime) UnmarshalJSON(b []byte) error {
	var parsed time.Time
	if err := json.Unmarshal(b, &parsed); err == nil {
		t.Time = parsed
	}
	return nil
}

// lenientInt64 decodes an optional integer. A value that is not a JSON
// integer in the int64 range leaves 0.
type lenientInt64 int64

// UnmarshalJSON never returns an error: an unreadable hint is "unknown".
func (n *lenientInt64) UnmarshalJSON(b []byte) error {
	var v int64
	if err := json.Unmarshal(b, &v); err == nil {
		*n = lenientInt64(v)
	}
	return nil
}

// lenientString decodes an optional string. A value of any other JSON type
// leaves "".
type lenientString string

// UnmarshalJSON never returns an error: an unreadable hint is "unknown".
func (s *lenientString) UnmarshalJSON(b []byte) error {
	var v string
	if err := json.Unmarshal(b, &v); err == nil {
		*s = lenientString(v)
	}
	return nil
}
