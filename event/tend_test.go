package event_test

import (
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/marsadhq/kit/event"
)

// legacyCoreEvent mirrors Tend's original event type: untagged fields, so
// encoding/json names the keys after the fields. It is what Tend's webhook
// put under "event" before kit existed.
type legacyCoreEvent struct {
	ID        int64
	OrgID     int64
	Type      string
	Source    string
	Payload   string
	DedupKey  string
	CreatedAt time.Time
}

// legacyBody is how Tend's webhook provider built its body.
func legacyBody(t *testing.T, subject, body string, ev legacyCoreEvent) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"subject": subject, "body": body, "event": ev})
	if err != nil {
		t.Fatalf("marshal legacy body: %v", err)
	}
	return b
}

func sampleTendEvent() event.TendEvent {
	return event.TendEvent{
		ID: 101, OrgID: 1, Type: "run.failed", Source: "jobs.runner",
		Payload:   `{"run_id":41,"job_id":7,"job_name":"backup","status":"failed","exit_code":1}`,
		DedupKey:  "run:41:run.failed",
		CreatedAt: t0,
	}
}

// TestTendWebhookEncodesLikeTend: without an envelope, a TendWebhook encodes
// to exactly the bytes Tend's old map-plus-struct encoding produced, for a
// stored event and for an old unstamped one (zero CreatedAt), and with
// characters encoding/json escapes.
func TestTendWebhookEncodesLikeTend(t *testing.T) {
	for name, ev := range map[string]event.TendEvent{
		"stored":    sampleTendEvent(),
		"unstamped": {OrgID: 1, Type: "heartbeat.missed", Source: "heartbeat", Payload: "db-backup"},
		"escaped":   {ID: 9, OrgID: 1, Type: "heartbeat.missed", Source: "heartbeat", Payload: `a<b>&"c"`, CreatedAt: t0},
	} {
		t.Run(name, func(t *testing.T) {
			subject, body := "Subject <x> & \"y\"", "line one\nline two"
			got, err := json.Marshal(event.TendWebhook{Subject: subject, Body: body, Event: &ev})
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			want := legacyBody(t, subject, body, legacyCoreEvent(ev))
			if string(got) != string(want) {
				t.Fatalf("encoding changed:\n got %s\nwant %s", got, want)
			}
		})
	}
}

// TestTendWebhookLegacyKeysNeverChange pins the key names, top level and in
// the event, with and without the envelope. Receivers in the field parse
// exactly these.
func TestTendWebhookLegacyKeysNeverChange(t *testing.T) {
	ev := sampleTendEvent()
	env := validV1()
	for name, wh := range map[string]event.TendWebhook{
		"legacy only":       {Subject: "s", Body: "b", Event: &ev},
		"legacy and marsad": {Subject: "s", Body: "b", Event: &ev, Marsad: &env},
	} {
		t.Run(name, func(t *testing.T) {
			b, err := json.Marshal(wh)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var top map[string]json.RawMessage
			if err := json.Unmarshal(b, &top); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			want := []string{"body", "event", "subject"}
			if wh.Marsad != nil {
				want = []string{"body", "event", "marsad", "subject"}
			}
			if got := keys(top); !reflect.DeepEqual(got, want) {
				t.Fatalf("top-level keys = %v, want %v", got, want)
			}
			var evKeys map[string]json.RawMessage
			if err := json.Unmarshal(top["event"], &evKeys); err != nil {
				t.Fatalf("event: %v", err)
			}
			if got, want := keys(evKeys), []string{"CreatedAt", "DedupKey", "ID", "OrgID", "Payload", "Source", "Type"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("event keys = %v, want %v", got, want)
			}
			// The legacy part is byte-identical whether or not the envelope
			// rides along.
			legacy := legacyBody(t, "s", "b", legacyCoreEvent(ev))
			var lt map[string]json.RawMessage
			_ = json.Unmarshal(legacy, &lt)
			for _, k := range []string{"body", "event", "subject"} {
				if string(top[k]) != string(lt[k]) {
					t.Errorf("%s = %s, want %s", k, top[k], lt[k])
				}
			}
		})
	}
}

// keys returns m's keys in sorted order.
func keys(m map[string]json.RawMessage) []string { return slices.Sorted(maps.Keys(m)) }

func TestParseTendWebhookRoundTrip(t *testing.T) {
	ev := sampleTendEvent()
	env := validV1()
	for name, want := range map[string]event.TendWebhook{
		"legacy only":       {Subject: "Job failed: backup", Body: "status=failed exit_code=1", Event: &ev},
		"legacy and marsad": {Subject: "Job failed: backup", Body: "status=failed exit_code=1", Event: &ev, Marsad: &env},
	} {
		t.Run(name, func(t *testing.T) {
			b, err := json.Marshal(want)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			got, err := event.ParseTendWebhook(b)
			if err != nil {
				t.Fatalf("ParseTendWebhook: %v", err)
			}
			if got.EnvelopeError() != nil {
				t.Fatalf("EnvelopeError: %v", got.EnvelopeError())
			}
			again, _ := json.Marshal(got)
			if string(again) != string(b) {
				t.Fatalf("re-encoded body differs:\n got %s\nwant %s", again, b)
			}
		})
	}
}

// TestParseTendWebhookLenientHints: the hint fields of the event decode the
// way Ward has always decoded them; a value it cannot read is unknown, never
// a rejected body.
func TestParseTendWebhookLenientHints(t *testing.T) {
	body := `{"subject":"s","body":"b","event":{"ID":"x","OrgID":1.5,"Type":"heartbeat.missed","Source":"heartbeat",` +
		`"Payload":"db-backup","DedupKey":42,"CreatedAt":"yesterday"}}`
	got, err := event.ParseTendWebhook([]byte(body))
	if err != nil {
		t.Fatalf("ParseTendWebhook: %v", err)
	}
	want := event.TendEvent{Type: "heartbeat.missed", Source: "heartbeat", Payload: "db-backup"}
	if *got.Event != want {
		t.Fatalf("event = %+v, want %+v", *got.Event, want)
	}

	// An old Tend sent the in-memory event with Go's zero time.
	old := `{"subject":"s","body":"b","event":{"ID":0,"OrgID":1,"Type":"run.failed","Source":"jobs.runner",` +
		`"Payload":"{}","DedupKey":"run:1:run.failed","CreatedAt":"0001-01-01T00:00:00Z"}}`
	got, err = event.ParseTendWebhook([]byte(old))
	if err != nil {
		t.Fatalf("ParseTendWebhook(old): %v", err)
	}
	if !got.Event.CreatedAt.IsZero() || got.Event.DedupKey != "run:1:run.failed" {
		t.Fatalf("old event = %+v", *got.Event)
	}
}

func TestParseTendWebhookRejects(t *testing.T) {
	for name, body := range map[string]string{
		"not JSON":           `{"subject":`,
		"subject not string": `{"subject":1,"body":"b","event":{"Type":"x.y"}}`,
		"body not string":    `{"subject":"s","body":{},"event":{"Type":"x.y"}}`,
		"event not object":   `{"subject":"s","body":"b","event":"run.failed"}`,
		"type not string":    `{"subject":"s","body":"b","event":{"Type":7}}`,
		"source not string":  `{"subject":"s","body":"b","event":{"Type":"x.y","Source":[]}}`,
		"payload not string": `{"subject":"s","body":"b","event":{"Type":"x.y","Payload":{"job_id":7}}}`,
	} {
		if _, err := event.ParseTendWebhook([]byte(body)); err == nil {
			t.Errorf("%s: accepted %s", name, body)
		}
	}
	for name, body := range map[string]string{
		"no event":   `{"title":"disk full","severity":"critical"}`,
		"null event": `{"subject":"s","body":"b","event":null}`,
	} {
		if _, err := event.ParseTendWebhook([]byte(body)); !errors.Is(err, event.ErrNotTendWebhook) {
			t.Errorf("%s: err = %v, want ErrNotTendWebhook", name, err)
		}
	}
}

// TestParseTendWebhookDropsABadEnvelope: an envelope that is not valid V1
// never costs the receiver the legacy body. Marsad is nil and EnvelopeError
// says why; a null envelope is simply absent.
func TestParseTendWebhookDropsABadEnvelope(t *testing.T) {
	legacy := `"subject":"s","body":"b","event":{"Type":"heartbeat.missed","Source":"heartbeat","Payload":"db-backup"}`
	for name, marsad := range map[string]string{
		"wrong type":   `"marsad":"yes"`,
		"wrong schema": `"marsad":{"schema":"marsad.event/v9"}`,
		"incomplete":   `"marsad":{"schema":"marsad.event/v1","id":"1"}`,
		"bad field":    `"marsad":{"schema":"marsad.event/v1","sequence":"soon"}`,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := event.ParseTendWebhook([]byte("{" + legacy + "," + marsad + "}"))
			if err != nil {
				t.Fatalf("ParseTendWebhook: %v", err)
			}
			if got.Marsad != nil {
				t.Fatalf("kept an invalid envelope: %+v", got.Marsad)
			}
			if got.EnvelopeError() == nil {
				t.Fatal("EnvelopeError = nil, want the reason")
			}
			if got.Event == nil || got.Event.Payload != "db-backup" || got.Subject != "s" {
				t.Fatalf("legacy fields lost: %+v", got)
			}
		})
	}
	got, err := event.ParseTendWebhook([]byte("{" + legacy + `,"marsad":null}`))
	if err != nil || got.Marsad != nil || got.EnvelopeError() != nil {
		t.Fatalf("null envelope: marsad=%v envErr=%v err=%v", got.Marsad, got.EnvelopeError(), err)
	}
}
