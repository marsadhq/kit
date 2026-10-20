package event_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/marsadhq/kit/event"
)

var t0 = time.Date(2026, 10, 9, 10, 0, 0, 297609739, time.UTC)

// validV1 is a complete, valid envelope.
func validV1() event.V1 {
	return event.V1{
		Schema:     event.SchemaV1,
		ID:         "103",
		Source:     event.Source{Tool: "tend", Instance: "primary"},
		Kind:       "heartbeat.missed",
		Subject:    event.Subject{Type: "heartbeat", ID: "db-backup", Name: "db-backup"},
		Severity:   event.SeverityCritical,
		Status:     event.StatusFiring,
		DedupKey:   "heartbeat.missed:db-backup@1791540000297609739",
		Sequence:   event.SequenceAt(t0),
		OccurredAt: t0,
		Title:      "Heartbeat missed: db-backup",
		Body:       "db-backup",
		Data:       json.RawMessage(`{"heartbeat":"db-backup"}`),
	}
}

// TestV1WireFormat pins the JSON form: lower-case keys in this order, the
// schema name, RFC 3339 nanosecond time, and data embedded as JSON.
func TestV1WireFormat(t *testing.T) {
	got, err := validV1().Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	const want = `{"schema":"marsad.event/v1","id":"103","source":{"tool":"tend","instance":"primary"},` +
		`"kind":"heartbeat.missed","subject":{"type":"heartbeat","id":"db-backup","name":"db-backup"},` +
		`"severity":"critical","status":"firing","dedup_key":"heartbeat.missed:db-backup@1791540000297609739",` +
		`"sequence":1791540000297609739,"occurred_at":"2026-10-09T10:00:00.297609739Z",` +
		`"title":"Heartbeat missed: db-backup","body":"db-backup","data":{"heartbeat":"db-backup"}}`
	if string(got) != want {
		t.Fatalf("Encode =\n%s\nwant\n%s", got, want)
	}
}

// TestV1OmitsOnlyEmptyData: data is the one key left out when empty; the
// optional strings and the zero sequence are still present.
func TestV1OmitsOnlyEmptyData(t *testing.T) {
	e := validV1()
	e.Data, e.Body, e.DedupKey, e.Sequence, e.Source.Instance, e.Subject.Name = nil, "", "", 0, "", ""
	got, err := e.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if strings.Contains(string(got), `"data"`) {
		t.Errorf("empty data was encoded: %s", got)
	}
	for _, key := range []string{`"body":""`, `"dedup_key":""`, `"sequence":0`, `"instance":""`, `"name":""`} {
		if !strings.Contains(string(got), key) {
			t.Errorf("missing %s in %s", key, got)
		}
	}
}

func TestV1RoundTrip(t *testing.T) {
	want := validV1()
	b, err := want.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := event.ParseV1(b)
	if err != nil {
		t.Fatalf("ParseV1: %v", err)
	}
	if !got.OccurredAt.Equal(want.OccurredAt) {
		t.Fatalf("occurred_at %v, want %v", got.OccurredAt, want.OccurredAt)
	}
	got.OccurredAt = want.OccurredAt
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, want)
	}
}

// TestParseV1IgnoresUnknownFields: v1 only ever gains fields, so a receiver
// must accept an envelope from a sender that knows more than it does.
func TestParseV1IgnoresUnknownFields(t *testing.T) {
	b, _ := validV1().Encode()
	b = []byte(strings.Replace(string(b), `"id":"103"`, `"id":"103","added_later":{"x":1}`, 1))
	if _, err := event.ParseV1(b); err != nil {
		t.Fatalf("ParseV1 with an unknown field: %v", err)
	}
}

func TestV1ValidateRejects(t *testing.T) {
	cases := map[string]func(*event.V1){
		"no schema":            func(e *event.V1) { e.Schema = "" },
		"other schema":         func(e *event.V1) { e.Schema = "marsad.event/v2" },
		"no id":                func(e *event.V1) { e.ID = "" },
		"no tool":              func(e *event.V1) { e.Source.Tool = "" },
		"no kind":              func(e *event.V1) { e.Kind = "" },
		"one-word kind":        func(e *event.V1) { e.Kind = "missed" },
		"upper-case kind":      func(e *event.V1) { e.Kind = "Heartbeat.Missed" },
		"kind with space":      func(e *event.V1) { e.Kind = "heartbeat. missed" },
		"empty kind segment":   func(e *event.V1) { e.Kind = "heartbeat..missed" },
		"no subject type":      func(e *event.V1) { e.Subject.Type = "" },
		"no subject id":        func(e *event.V1) { e.Subject.ID = "" },
		"unknown severity":     func(e *event.V1) { e.Severity = "major" },
		"no severity":          func(e *event.V1) { e.Severity = "" },
		"unknown status":       func(e *event.V1) { e.Status = "acknowledged" },
		"no status":            func(e *event.V1) { e.Status = "" },
		"negative sequence":    func(e *event.V1) { e.Sequence = -1 },
		"no occurred_at":       func(e *event.V1) { e.OccurredAt = time.Time{} },
		"no title":             func(e *event.V1) { e.Title = "" },
		"data not JSON":        func(e *event.V1) { e.Data = json.RawMessage(`{"a":`) },
		"data an array":        func(e *event.V1) { e.Data = json.RawMessage(`[1,2]`) },
		"data a string":        func(e *event.V1) { e.Data = json.RawMessage(`"db-backup"`) },
		"data null":            func(e *event.V1) { e.Data = json.RawMessage(`null`) },
		"data only whitespace": func(e *event.V1) { e.Data = json.RawMessage(`  `) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := validV1()
			mutate(&e)
			if err := e.Validate(); err == nil {
				t.Fatalf("Validate accepted %+v", e)
			}
			if _, err := e.Encode(); err == nil {
				t.Fatal("Encode accepted an invalid envelope")
			}
		})
	}
}

func TestV1ValidateAccepts(t *testing.T) {
	for name, mutate := range map[string]func(*event.V1){
		"as built":          func(*event.V1) {},
		"three-part kind":   func(e *event.V1) { e.Kind = "notification.delivery.failed" },
		"kind with _ and -": func(e *event.V1) { e.Kind = "heart_beat.re-covered" },
		"no data":           func(e *event.V1) { e.Data = nil },
		"spaced object":     func(e *event.V1) { e.Data = json.RawMessage(" \n{}") },
		"info status":       func(e *event.V1) { e.Status = event.StatusInfo },
		"warning":           func(e *event.V1) { e.Severity = event.SeverityWarning },
		"info severity":     func(e *event.V1) { e.Severity = event.SeverityInfo },
		"resolved":          func(e *event.V1) { e.Status = event.StatusResolved },
		"no optional parts": func(e *event.V1) {
			e.Body, e.DedupKey, e.Sequence, e.Source.Instance, e.Subject.Name = "", "", 0, "", ""
		},
	} {
		e := validV1()
		mutate(&e)
		if err := e.Validate(); err != nil {
			t.Errorf("%s: Validate: %v", name, err)
		}
	}
}

func TestParseV1Rejects(t *testing.T) {
	for name, in := range map[string]string{
		"not JSON":         `{"schema":`,
		"wrong type":       `{"schema":"marsad.event/v1","sequence":"12"}`,
		"invalid envelope": `{"schema":"marsad.event/v1"}`,
	} {
		if _, err := event.ParseV1([]byte(in)); err == nil {
			t.Errorf("%s: ParseV1 accepted %s", name, in)
		}
	}
}

func TestSequenceAt(t *testing.T) {
	for _, c := range []struct {
		at   time.Time
		want int64
	}{
		{t0, t0.UnixNano()},
		{time.Unix(0, 1), 1},
		{time.Time{}, 0},
		{time.Date(1969, 12, 31, 0, 0, 0, 0, time.UTC), 0},
		{time.Date(2262, 1, 1, 0, 0, 0, 0, time.UTC), 0},
	} {
		if got := event.SequenceAt(c.at); got != c.want {
			t.Errorf("SequenceAt(%v) = %d, want %d", c.at, got, c.want)
		}
	}
	// A later instant always has a larger sequence.
	if event.SequenceAt(t0.Add(time.Nanosecond)) <= event.SequenceAt(t0) {
		t.Error("SequenceAt is not increasing")
	}
}
