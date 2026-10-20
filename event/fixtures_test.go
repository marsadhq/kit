package event_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/marsadhq/kit/event"
)

var update = flag.Bool("update", false, "rewrite the golden fixtures under testdata/tend")

// tendCase is one Tend event as its webhook channel delivers it, with the
// envelope fields Tend's mapping (see fixtures.go) gives it.
type tendCase struct {
	ev            event.TendEvent
	subject, body string
	subj          event.Subject
	sev           event.Severity
	status        event.Status
	data          string
}

// fixtureT0 is the stored time of the first fixture event.
var fixtureT0 = time.Date(2026, 10, 9, 10, 0, 0, 297609739, time.UTC)

// tendCases is the canonical table the fixtures are built from. Subjects and
// bodies are what Tend renders for each type (internal/notify messageFor);
// sources, payloads and dedup keys are what its emitters record
// (internal/jobs runner and RemovedEvent, internal/heartbeat, and the notify
// worker's give-up).
func tendCases() map[string]tendCase {
	at := func(m int) time.Time { return fixtureT0.Add(time.Duration(m) * time.Minute) }
	job := event.Subject{Type: "job", ID: "7", Name: "backup"}
	hb := event.Subject{Type: "heartbeat", ID: "db-backup", Name: "db-backup"}
	return map[string]tendCase{
		"run.failed": {
			ev: event.TendEvent{ID: 101, OrgID: 1, Type: "run.failed", Source: "jobs.runner",
				Payload:  `{"run_id":41,"job_id":7,"job_name":"backup","status":"failed","exit_code":1}`,
				DedupKey: "run:41:run.failed", CreatedAt: at(0)},
			subject: "Job failed: backup", body: "status=failed exit_code=1",
			subj: job, sev: event.SeverityCritical, status: event.StatusFiring,
			data: `{"run_id":41,"job_id":7,"job_name":"backup","status":"failed","exit_code":1}`,
		},
		"run.recovered": {
			ev: event.TendEvent{ID: 102, OrgID: 1, Type: "run.recovered", Source: "jobs.runner",
				Payload:  `{"run_id":42,"job_id":7,"job_name":"backup","status":"succeeded","exit_code":0}`,
				DedupKey: "run:42:run.recovered", CreatedAt: at(1)},
			subject: "Job recovered: backup", body: "status=succeeded exit_code=0; the previous run had not succeeded",
			subj: job, sev: event.SeverityCritical, status: event.StatusResolved,
			data: `{"run_id":42,"job_id":7,"job_name":"backup","status":"succeeded","exit_code":0}`,
		},
		"heartbeat.missed": {
			ev:      event.TendEvent{ID: 103, OrgID: 1, Type: "heartbeat.missed", Source: "heartbeat", Payload: "db-backup", CreatedAt: at(2)},
			subject: "Heartbeat missed: db-backup", body: "db-backup",
			subj: hb, sev: event.SeverityCritical, status: event.StatusFiring,
			data: `{"heartbeat":"db-backup"}`,
		},
		"heartbeat.recovered": {
			ev:      event.TendEvent{ID: 104, OrgID: 1, Type: "heartbeat.recovered", Source: "heartbeat", Payload: "db-backup", CreatedAt: at(3)},
			subject: "Heartbeat recovered: db-backup", body: "db-backup",
			subj: hb, sev: event.SeverityCritical, status: event.StatusResolved,
			data: `{"heartbeat":"db-backup"}`,
		},
		"job.removed": {
			ev: event.TendEvent{ID: 105, OrgID: 1, Type: "job.removed", Source: "jobs",
				Payload: `{"job_id":7,"job_name":"backup"}`, DedupKey: "job:7:removed", CreatedAt: at(4)},
			subject: "Job removed: backup", body: "the job was deleted; it will not run or alert again",
			subj: job, sev: event.SeverityInfo, status: event.StatusResolved,
			data: `{"job_id":7,"job_name":"backup"}`,
		},
		"heartbeat.removed": {
			ev:      event.TendEvent{ID: 106, OrgID: 1, Type: "heartbeat.removed", Source: "heartbeat", Payload: "db-backup", CreatedAt: at(5)},
			subject: "Heartbeat removed: db-backup", body: "the heartbeat was deleted; it is no longer monitored",
			subj: hb, sev: event.SeverityInfo, status: event.StatusResolved,
			data: `{"heartbeat":"db-backup"}`,
		},
		"notification.failed": {
			ev:      event.TendEvent{ID: 107, OrgID: 1, Type: "notification.failed", Source: "notify", Payload: "heartbeat.missed", CreatedAt: at(6)},
			subject: "Alert delivery failed: heartbeat.missed",
			body:    "a heartbeat.missed notification could not be delivered and was given up after retrying; that alert was not received",
			subj:    event.Subject{Type: "notifier", ID: "notify", Name: "notify"},
			sev:     event.SeverityWarning, status: event.StatusFiring,
			data: `{"undelivered":"heartbeat.missed"}`,
		},
	}
}

// envelope builds c's V1 envelope by Tend's mapping.
func (c tendCase) envelope() event.V1 {
	base := c.ev.DedupKey
	if base == "" {
		base = c.ev.Type + ":" + c.ev.Payload
	}
	return event.V1{
		Schema:     event.SchemaV1,
		ID:         strconv.FormatInt(c.ev.ID, 10),
		Source:     event.Source{Tool: "tend", Instance: "primary"},
		Kind:       c.ev.Type,
		Subject:    c.subj,
		Severity:   c.sev,
		Status:     c.status,
		DedupKey:   fmt.Sprintf("%s@%d", base, c.ev.CreatedAt.UnixNano()),
		Sequence:   event.SequenceAt(c.ev.CreatedAt),
		OccurredAt: c.ev.CreatedAt,
		Title:      c.subject,
		Body:       c.body,
		Data:       json.RawMessage(c.data),
	}
}

// webhook builds c's body, with or without the envelope.
func (c tendCase) webhook(withEnvelope bool) event.TendWebhook {
	ev := c.ev
	w := event.TendWebhook{Subject: c.subject, Body: c.body, Event: &ev}
	if withEnvelope {
		env := c.envelope()
		w.Marsad = &env
	}
	return w
}

// TestTendFixtures checks every golden body against the canonical table, or
// rewrites them all with -update.
func TestTendFixtures(t *testing.T) {
	cases := tendCases()
	if got, want := slices.Sorted(maps.Keys(cases)), slices.Sorted(slices.Values(event.TendEventTypes())); !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical table covers %v, fixtures list %v", got, want)
	}
	for _, typ := range event.TendEventTypes() {
		for _, env := range []bool{false, true} {
			c := cases[typ]
			want, err := json.Marshal(c.webhook(env))
			if err != nil {
				t.Fatalf("%s: marshal: %v", typ, err)
			}
			name := event.TendFixtureName(typ, env)
			if *update {
				if err := os.WriteFile(filepath.Join("testdata", "tend", name), append(want, '\n'), 0o644); err != nil {
					t.Fatalf("write %s: %v", name, err)
				}
				continue
			}
			got, err := event.LoadTendFixture(typ, env)
			if err != nil {
				t.Fatalf("LoadTendFixture(%s, %v): %v", typ, env, err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs from the canonical table:\n got %s\nwant %s\n(run with -update only if the change is intended)", name, got, want)
			}
		}
	}
}

// TestTendLegacyFixturesAreTendsEncoding: each legacy fixture is exactly what
// Tend's original encoding (a map around its untagged event struct) produced.
func TestTendLegacyFixturesAreTendsEncoding(t *testing.T) {
	for typ, c := range tendCases() {
		got, err := event.LoadTendFixture(typ, false)
		if err != nil {
			t.Fatalf("LoadTendFixture: %v", err)
		}
		if want := legacyBody(t, c.subject, c.body, legacyCoreEvent(c.ev)); !bytes.Equal(got, want) {
			t.Errorf("%s:\n got %s\nwant %s", typ, got, want)
		}
	}
}

// TestTendEnvelopeFixturesKeepTheLegacyBody: adding the envelope changes
// nothing else, so a receiver that ignores "marsad" sees the legacy body.
func TestTendEnvelopeFixturesKeepTheLegacyBody(t *testing.T) {
	for _, typ := range event.TendEventTypes() {
		legacy, _ := event.LoadTendFixture(typ, false)
		withEnv, _ := event.LoadTendFixture(typ, true)
		var a, b map[string]json.RawMessage
		if err := json.Unmarshal(legacy, &a); err != nil {
			t.Fatalf("%s legacy: %v", typ, err)
		}
		if err := json.Unmarshal(withEnv, &b); err != nil {
			t.Fatalf("%s marsad: %v", typ, err)
		}
		if _, ok := b["marsad"]; !ok {
			t.Fatalf("%s: envelope fixture has no marsad field", typ)
		}
		delete(b, "marsad")
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s: the envelope fixture's legacy fields differ from the legacy fixture", typ)
		}
	}
}

// TestTendFixturesParse: every fixture decodes; the envelope ones carry a
// valid envelope that agrees with the legacy event.
func TestTendFixturesParse(t *testing.T) {
	fixtures := event.TendFixtures()
	if len(fixtures) != 2*len(event.TendEventTypes()) {
		t.Fatalf("TendFixtures returned %d, want %d", len(fixtures), 2*len(event.TendEventTypes()))
	}
	for _, f := range fixtures {
		w, err := event.ParseTendWebhook(f.Body)
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		if w.Event.Type != f.Type || w.Event.CreatedAt.IsZero() {
			t.Errorf("%s: event %+v", f.Name, *w.Event)
		}
		if !f.Envelope {
			if w.Marsad != nil {
				t.Errorf("%s: legacy fixture carries an envelope", f.Name)
			}
			continue
		}
		if w.Marsad == nil {
			t.Fatalf("%s: envelope not decoded: %v", f.Name, w.EnvelopeError())
		}
		m := *w.Marsad
		if m.Kind != w.Event.Type || m.Title != w.Subject || m.Body != w.Body ||
			!m.OccurredAt.Equal(w.Event.CreatedAt) || m.Sequence != w.Event.CreatedAt.UnixNano() ||
			m.ID != strconv.FormatInt(w.Event.ID, 10) || m.Source.Tool != "tend" {
			t.Errorf("%s: envelope disagrees with the legacy event: %+v", f.Name, m)
		}
	}
}

// TestTendFixtureFilesAreExactlyTheSet: testdata/tend holds the two bodies of
// each event type and nothing else, so no stray file can pass for a fixture.
func TestTendFixtureFilesAreExactlyTheSet(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("testdata", "tend"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var got, want []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	for _, f := range event.TendFixtures() {
		want = append(want, f.Name)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("testdata/tend holds %v, want exactly %v", got, want)
	}
}

func TestLoadTendFixtureUnknownType(t *testing.T) {
	for _, typ := range []string{"run.started", "run.succeeded", "", "../tend/run.failed"} {
		if _, err := event.LoadTendFixture(typ, false); err == nil {
			t.Errorf("LoadTendFixture(%q) found a fixture", typ)
		}
	}
	if b, err := event.LoadTendFixture("run.failed", false); err != nil || bytes.HasSuffix(b, []byte("\n")) {
		t.Errorf("LoadTendFixture(run.failed) = %q, %v; want the body without the trailing newline", b, err)
	}
	if _, err := event.ParseTendWebhook([]byte(`{}`)); !errors.Is(err, event.ErrNotTendWebhook) {
		t.Errorf("empty object: %v", err)
	}
}
