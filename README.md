# kit

`github.com/marsadhq/kit` is the shared Go module of the Marsad suite.

Marsad (Arabic for "observatory") is a set of single-binary, self-hosted
operations tools: [Tend](https://github.com/marsadhq/tend), a job runner and
heartbeat monitor, and Ward, an on-call and incident tool, with more to
follow. Each tool used to carry its own copy of the code they share, so a fix
landed in one copy only. kit holds that code once: building blocks with no
knowledge of any tool's jobs, incidents, users or tables. Each tool keeps its
own domain and imports these.

## Packages

| Package | What it is |
| --- | --- |
| [`secrets`](secrets) | `Box`: AES-256-GCM sealing of small secret values under a base64 32-byte master key. |
| [`clock`](clock) | `Clock`, with `RealClock` and a `FakeClock` for tests. |
| [`auth`](auth) | argon2id `HashPassword`/`VerifyPassword`, `VerifyPasswordOrDummy` for login forms (an unknown or passwordless user still costs a full check), API tokens with the tool's prefix (`GenerateToken("tend_")`, `HashToken`), and the HMAC `SessionCodec` for session cookies and CSRF tokens. |
| [`urlredact`](urlredact) | `Host` and `Error`: reduce a URL to its host and an error to its cause, so a token in a URL never reaches a log. |
| [`httpx`](httpx) | `SecurityHeaders` (CSP and friends), `NewServer` with every connection phase bounded, the keyed token-bucket `RateLimiter` with `ClientIP`, and the `LoginLimiter` (consecutive-failure backoff per address and account). |
| [`notify`](notify) | The `Provider` contract and `Message` (subject, body, untrusted output, priority, action buttons), providers for webhooks, Slack (text escaped, so it cannot mention or link under a label of its own; untrusted output in a code block), Discord (no mention pings, link and mention syntax escaped, untrusted output in a code block, held to Discord's 2000-unit limit), SMTP, Telegram (inline keyboards, HTML text, and editing or deleting a sent message), ntfy (replacing or clearing a sent message by its sequence id) and Pushover (the receipt and tags of an emergency message, and cancelling its repeats by either), `BuildProvider` from a channel's JSON config, permanent-error classification and the HTTP status of a refusal, and the durable delivery `Worker`, which drives a tool's own queue through the `Queue` interface. |
| [`event`](event) | The Marsad event format: the `V1` envelope, Tend's legacy webhook body `TendWebhook` (which can carry the envelope), and golden fixtures for every Tend event type. |

The only dependency outside the standard library is `golang.org/x/crypto`
(for argon2).

## The event format

A `marsad.event/v1` envelope is JSON with lower-case keys:

```json
{
  "schema": "marsad.event/v1",
  "id": "103",
  "source": {"tool": "tend", "instance": "primary"},
  "kind": "heartbeat.missed",
  "subject": {"type": "heartbeat", "id": "db-backup", "name": "db-backup"},
  "severity": "critical",
  "status": "firing",
  "dedup_key": "heartbeat.missed:db-backup@1791540120297609739",
  "sequence": 1791540120297609739,
  "occurred_at": "2026-10-09T10:02:00.297609739Z",
  "title": "Heartbeat missed: db-backup",
  "body": "db-backup",
  "data": {"heartbeat": "db-backup"}
}
```

`severity` is `critical`, `warning` or `info`; `status` is `firing`,
`resolved` or `info` (a record that opens and closes nothing). `dedup_key`
identifies the occurrence, so a redelivery can be recognised; `sequence` is
an ordering hint (Tend uses the event time in nanoseconds). `data` is an
optional JSON object.

Tend's webhook body keeps its legacy keys exactly
(`{"subject", "body", "event": {"ID", "OrgID", "Type", "Source", "Payload",
"DedupKey", "CreatedAt"}}`) and adds the envelope under `"marsad"`, so a
receiver that only knows the legacy shape keeps working and one that knows
the envelope can prefer it. The files in `event/testdata/tend` are the
contract: Tend's tests assert it produces them byte for byte, Ward's that it
accepts them. `event.LoadTendFixture` and `event.TendFixtures` load them from
any module's tests.

## Stability

kit is v0.x: its Go API may still change between minor versions, and the
tools pin the version they use.

The event format is versioned separately and changes more carefully. Within
`marsad.event/v1` fields are only ever added, never renamed, removed or given
a new meaning, so a v1 receiver ignores what it does not know. Anything else
is a new schema. The legacy Tend webhook keys never change.

## Development

Go 1.26 or later.

```sh
gofmt -l .                   # prints nothing
go vet ./...
go test ./... -race -count=1
```

The golden Tend fixtures are generated from the canonical table in
`event/fixtures_test.go`:

```sh
go test ./event -run '^TestTendFixtures$' -update
```

A change to an existing legacy fixture breaks every receiver; review the diff
before committing one.

## License

AGPL-3.0, like Tend. See [LICENSE](LICENSE).
