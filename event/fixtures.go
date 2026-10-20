package event

import (
	"bytes"
	"embed"
	"fmt"
	"slices"
)

//go:embed testdata/tend/*.legacy.json testdata/tend/*.marsad.json
var tendFixtureFS embed.FS

// tendEventTypes are the Tend event types a notification rule can route, in
// Tend's documentation order.
var tendEventTypes = []string{
	"run.failed",
	"run.recovered",
	"heartbeat.missed",
	"heartbeat.recovered",
	"job.removed",
	"heartbeat.removed",
	"notification.failed",
}

// TendEventTypes returns the Tend event types that have golden fixtures,
// which are the types Tend delivers to notification channels. The slice is a
// copy.
func TendEventTypes() []string { return slices.Clone(tendEventTypes) }

// TendFixture is one golden webhook body. The golden bodies under
// testdata/tend are the contract between Tend's webhook channel and its
// receivers: Tend's tests assert it produces them byte for byte, and Ward's
// tests assert it accepts them. There are two per Tend event type:
// <type>.legacy.json, the body Tend has always sent, and <type>.marsad.json,
// the same body with the V1 envelope added.
//
// They are regenerated with `go test ./event -run '^TestTendFixtures$' -update`,
// which writes what the canonical table in fixtures_test.go builds. A change
// to an existing legacy fixture is a breaking change for every receiver.
//
// Tend maps its events onto V1 as follows. source is {"tool":"tend",
// "instance":<the instance name>}; id is the event's row id in decimal; kind
// is the event type; title and body are the webhook's subject and body;
// occurred_at is the event's CreatedAt and sequence is SequenceAt of it.
// dedup_key is the event's DedupKey, or "<type>:<payload>" when it has none,
// followed by "@" and CreatedAt in unix nanoseconds: the delivery identity
// Ward already derives from a stamped legacy event, so a receiver switching
// to the envelope still recognises redeliveries of events it saw in legacy
// form.
//
//	type                 subject (type, id, name)          severity  status    data
//	run.failed           job, job_id, job_name             critical  firing    the run payload
//	run.recovered        job, job_id, job_name             critical  resolved  the run payload
//	heartbeat.missed     heartbeat, name, name             critical  firing    {"heartbeat":name}
//	heartbeat.recovered  heartbeat, name, name             critical  resolved  {"heartbeat":name}
//	job.removed          job, job_id, job_name             info      resolved  the job payload
//	heartbeat.removed    heartbeat, name, name             info      resolved  {"heartbeat":name}
//	notification.failed  notifier, "notify", "notify"      warning   firing    {"undelivered":type}
//
// A heartbeat's name is its identity in Tend (unique per org, and what its
// events carry), so it is the subject id as well as its name.
// notification.failed is about Tend's alert delivery as a whole: every alert
// Tend gives up on belongs to one problem, as Ward groups it today.
type TendFixture struct {
	Type     string // the Tend event type, e.g. "run.failed"
	Envelope bool   // whether the body carries the "marsad" envelope
	Name     string // the file name under event/testdata/tend
	Body     []byte // the exact bytes Tend posts
}

// TendFixtureName is the file name of the fixture for eventType, with or
// without the envelope.
func TendFixtureName(eventType string, envelope bool) string {
	if envelope {
		return eventType + ".marsad.json"
	}
	return eventType + ".legacy.json"
}

// LoadTendFixture returns the golden body Tend posts for an event of
// eventType, with or without the V1 envelope. The bytes are exactly what Tend
// sends (the file's trailing newline is not part of them).
func LoadTendFixture(eventType string, envelope bool) ([]byte, error) {
	if !slices.Contains(tendEventTypes, eventType) {
		return nil, fmt.Errorf("event: no Tend fixture for event type %q", eventType)
	}
	b, err := tendFixtureFS.ReadFile("testdata/tend/" + TendFixtureName(eventType, envelope))
	if err != nil {
		return nil, fmt.Errorf("event: %w", err)
	}
	return bytes.TrimSuffix(b, []byte("\n")), nil
}

// TendFixtures returns every golden body: for each type in TendEventTypes
// order, the legacy body and then the one with the envelope.
func TendFixtures() []TendFixture {
	var out []TendFixture
	for _, typ := range tendEventTypes {
		for _, env := range []bool{false, true} {
			b, err := LoadTendFixture(typ, env)
			if err != nil {
				// The fixtures are embedded at build time, and a test checks
				// every one is present, so this cannot happen in a build.
				panic(err)
			}
			out = append(out, TendFixture{Type: typ, Envelope: env, Name: TendFixtureName(typ, env), Body: b})
		}
	}
	return out
}
