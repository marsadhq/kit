// Package event defines the Marsad event format: the V1 envelope one tool
// sends another (Tend reporting a missed heartbeat to Ward, say), and the
// legacy webhook body Tend has always sent, which can now carry the envelope
// alongside its old fields.
//
// The format is versioned. An envelope names its schema ("marsad.event/v1");
// within v1, fields are only ever added, never renamed, removed or given a
// new meaning, so a v1 receiver can ignore what it does not know. A change
// that breaks that promise is a new schema, v2, with its own type.
package event

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// SchemaV1 is the schema name every V1 envelope carries.
const SchemaV1 = "marsad.event/v1"

// Severity is how bad the problem an event is about is, or was.
type Severity string

const (
	SeverityCritical Severity = "critical" // something is down or failing; page someone
	SeverityWarning  Severity = "warning"  // degraded or at risk; look soon
	SeverityInfo     Severity = "info"     // nothing to act on
)

// Valid reports whether s is one of the defined severities.
func (s Severity) Valid() bool {
	switch s {
	case SeverityCritical, SeverityWarning, SeverityInfo:
		return true
	}
	return false
}

// Status says what an event does to the problem it is about.
type Status string

const (
	StatusFiring   Status = "firing"   // the problem starts or continues
	StatusResolved Status = "resolved" // the problem is over (fixed, or its subject is gone)
	StatusInfo     Status = "info"     // a record only: it opens nothing and closes nothing
)

// Valid reports whether s is one of the defined statuses.
func (s Status) Valid() bool {
	switch s {
	case StatusFiring, StatusResolved, StatusInfo:
		return true
	}
	return false
}

// Source names the tool installation an event comes from.
type Source struct {
	// Tool is the sending tool, lower case: "tend", "ward".
	Tool string `json:"tool"`
	// Instance tells installations of one tool apart (an operator-chosen
	// name, a hostname). It may be empty when there is only one.
	Instance string `json:"instance"`
}

// Subject is the thing an event is about: a job, a heartbeat, a service.
// Events with the same Source, Subject.Type and Subject.ID are about the same
// thing, which is what a receiver groups on.
type Subject struct {
	// Type is the kind of thing, lower case: "job", "heartbeat".
	Type string `json:"type"`
	// ID identifies it at its source and does not change when it is renamed.
	ID string `json:"id"`
	// Name is its display name. It may change and may be empty.
	Name string `json:"name"`
}

// V1 is the version 1 Marsad event envelope. Its JSON form has lower-case
// keys; every field is always present except data, which is omitted when
// there is none.
type V1 struct {
	// Schema is always SchemaV1.
	Schema string `json:"schema"`
	// ID is the event's identifier at its source, for display and support.
	// A source may issue an ID again (after its database is restored, say),
	// so receivers deduplicate on DedupKey, not on ID.
	ID string `json:"id"`
	// Source is the tool installation that sent the event.
	Source Source `json:"source"`
	// Kind is what happened, as lower-case dotted words naming the subject
	// type and then the change: "heartbeat.missed", "run.failed".
	Kind string `json:"kind"`
	// Subject is what the event is about.
	Subject Subject `json:"subject"`
	// Severity is how bad the problem is or was.
	Severity Severity `json:"severity"`
	// Status says whether the problem is firing, resolved, or neither.
	Status Status `json:"status"`
	// DedupKey identifies this occurrence: a redelivery of the event carries
	// the same key, and two different events never share one. Empty means
	// the sender cannot tell (receivers then cannot recognise a redelivery).
	DedupKey string `json:"dedup_key"`
	// Sequence orders events about one subject: a later event has a larger
	// value. 0 means unknown. It is a hint for discarding a stale event that
	// arrives after a newer one, not a counter: Tend uses the event's time in
	// unix nanoseconds (see SequenceAt).
	Sequence int64 `json:"sequence"`
	// OccurredAt is when the event happened at its source, encoded as
	// RFC 3339 with nanoseconds.
	OccurredAt time.Time `json:"occurred_at"`
	// Title is a one-line human summary.
	Title string `json:"title"`
	// Body is optional detail text.
	Body string `json:"body"`
	// Data is optional machine-readable detail: a JSON object whose shape
	// depends on Kind. Empty means none.
	Data json.RawMessage `json:"data,omitempty"`
}

// kindPattern is two or more dot-separated words of lower-case letters,
// digits, '_' and '-'.
var kindPattern = regexp.MustCompile(`^[a-z0-9_-]+(\.[a-z0-9_-]+)+$`)

// Validate reports the first way e is not a valid V1 envelope, or nil.
func (e V1) Validate() error {
	switch {
	case e.Schema != SchemaV1:
		return fmt.Errorf("event: schema %q, want %q", e.Schema, SchemaV1)
	case e.ID == "":
		return errors.New("event: missing id")
	case e.Source.Tool == "":
		return errors.New("event: missing source.tool")
	case !kindPattern.MatchString(e.Kind):
		return fmt.Errorf("event: kind %q is not lower-case dotted words (like heartbeat.missed)", e.Kind)
	case e.Subject.Type == "":
		return errors.New("event: missing subject.type")
	case e.Subject.ID == "":
		return errors.New("event: missing subject.id")
	case !e.Severity.Valid():
		return fmt.Errorf("event: severity %q, want critical, warning or info", e.Severity)
	case !e.Status.Valid():
		return fmt.Errorf("event: status %q, want firing, resolved or info", e.Status)
	case e.Sequence < 0:
		return fmt.Errorf("event: negative sequence %d", e.Sequence)
	case e.OccurredAt.IsZero():
		return errors.New("event: missing occurred_at")
	case e.Title == "":
		return errors.New("event: missing title")
	}
	if len(e.Data) > 0 {
		if !json.Valid(e.Data) {
			return errors.New("event: data is not valid JSON")
		}
		if d := bytes.TrimLeft(e.Data, " \t\r\n"); len(d) == 0 || d[0] != '{' {
			return errors.New("event: data is not a JSON object")
		}
	}
	return nil
}

// Encode validates e and returns its JSON form.
func (e V1) Encode() ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(e)
}

// ParseV1 decodes a V1 envelope and validates it. Fields it does not know are
// ignored, so envelopes from a newer v1 sender still parse.
func ParseV1(data []byte) (V1, error) {
	var e V1
	if err := json.Unmarshal(data, &e); err != nil {
		return V1{}, fmt.Errorf("event: %w", err)
	}
	if err := e.Validate(); err != nil {
		return V1{}, err
	}
	return e, nil
}

// SequenceAt is the Sequence for an event that happened at t: t in unix
// nanoseconds, or 0 (unknown) for the zero time and for any instant with no
// int64 nanosecond value after the epoch (before 1970 or after 2261).
func SequenceAt(t time.Time) int64 {
	if t.IsZero() || t.Year() < 1970 || t.Year() > 2261 {
		return 0
	}
	return t.UnixNano()
}
