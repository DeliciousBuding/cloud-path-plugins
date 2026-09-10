# cloud-path-app-button-indicator

[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![CI](https://github.com/DeliciousBuding/cloud-path-app-button-indicator/actions/workflows/ci.yml/badge.svg)](https://github.com/DeliciousBuding/cloud-path-app-button-indicator/actions/workflows/ci.yml)

A capability-only CloudPath **Application plugin** with a default walking light
and an opt-in **service-call / acknowledge** workflow for duty desks and
workstation requests. It is not a medical emergency or life-safety system.

Version **0.2.2** requires **Core v0.2.29+** (Core <0.3.0) and public SDK v0.2.15+.
Status: **IMPLEMENTED** — package and Application Protocol tests are not
real-device acceptance evidence.

## Installation and quick start

1. Download `plugin.yaml` and the matching
   `cloud-path-app-button-indicator_vX.Y.Z_<os>_<arch>[.exe]` asset from the
   same GitHub Release.
2. Verify both downloads with that Release's `checksums.txt`; do not mix a
   manifest and binary from different versions.
3. Install both through CloudPath Core's supported plugin installation flow.
   The binary is hosted by Core Plugin Host, not run as a standalone service.
4. Enable an instance, bind the required capabilities, and configure the mode
   described below. The default remains the silent walking light.

## WebUI contribution

`plugin.yaml` declares `ui.apiVersion: 1`. Core creates the navigation entry
**工位呼叫（值班台）** and the stable route `/apps/service-desk` for an enabled
instance.
The page is declarative: instance status, metrics from `service_call`
records, manual `acknowledge-pending` / `request` / `acknowledge` actions, the
`service_call` record card view, and a configuration form covering mode,
timezone, heartbeat cron and beep. Raw `app_config` / `app_bindings` remain
available only in advanced details.

## Capability requirements

The application knows no hardware, Driver ID, port or vendor-specific field.
All entity IDs are opaque values supplied by Core's Capability Binder.

| Requirement | Capability | Cardinality | Purpose |
|---|---|---|---|
| `button-input` | `cloudpath.dev/capability/key@1` | one-or-more, minItems 1 | request button(s), or legacy walking-light input |
| `indicator` | `cloudpath.dev/capability/led@1` | one | LED-bank output |
| `sound` | `cloudpath.dev/capability/buzzer@1` | zero-or-one | optional beep, disabled by default |
| `acknowledge-input` | `cloudpath.dev/capability/key@1` | zero-or-one | physical acknowledgement button in call mode |

Request and acknowledgement inputs must be different entities. Omitting
`acknowledge-input` is supported: use the management-console acknowledgement job.
One instance has **one outstanding call**, not a queue per input. Bind separate
instances for independent workstation queues, and dedicate an indicator bank to
each instance so unrelated applications cannot overwrite its output.

## Modes and configuration

All configuration fields are optional. Defaults remain `walking-light`, UTC,
`*/5 * * * *`, and no beep. Unknown modes are rejected; `call` is an alias for
`service-call`.

```json
{
  "mode": "service-call",
  "timezone": "Asia/Shanghai",
  "heartbeat_cron": "*/5 * * * *",
  "beep_on_press": false
}
```

- **Default / `walking-light`:** each bound key press advances
  `1 → 2 → 4 → … → 128 → 1` and upserts the legacy `press/last` record. The
  original fire-and-forget walking-light behavior is unchanged.
- **`service-call` / `call`:** a request creates a uniquely identified
  `service_call` domain record with `status: pending` and requests LED mask
  `255`. A physical acknowledgement or the exact-ID acknowledgement job records
  `status: acknowledged` and requests mask `0`. Dispatch alone never proves the
  lamp changed; inspect the separate command results below.
- **Debounce / coalescing:** repeated event sequences are ignored, physical
  presses have a 250 ms per-entity debounce, and all requests while one is pending
  resolve to that same call without repeating commands or creating records.
  There is no automatic acknowledgement or business timeout. A deliberate new
  request after acknowledgement gets a different ID.
- **Optional sound:** `beep_on_press: true` plus a `sound` binding emits one short
  beep per newly created call, not on coalesced presses or acknowledgement.
- **Heartbeat:** the existing quiet five-minute cron remains enabled. The
  automatically driven `bootstrap` job registers it once per configuration
  revision. `indicator-heartbeat` remains exclusively cron-owned; it is not a
  second automatically driven descriptor job. Call-mode heartbeats include the
  pending request ID and actual command-result state, not an assumed LED mask.

### Configure the desired map: `app_config` + `app_bindings`

Both desired-map values are **JSON strings**, not nested objects/arrays.
Choose exact stable entity IDs from Core's entity/capability picker first, then
replace the three placeholder values in this example:

```json
{
  "app_config": "{\"mode\":\"service-call\",\"timezone\":\"Asia/Shanghai\",\"heartbeat_cron\":\"*/5 * * * *\",\"beep_on_press\":false}",
  "app_bindings": "[{\"requirement_id\":\"button-input\",\"entity_id\":\"REQUEST_ENTITY_ID\"},{\"requirement_id\":\"acknowledge-input\",\"entity_id\":\"ACK_ENTITY_ID\"},{\"requirement_id\":\"indicator\",\"entity_id\":\"LED_ENTITY_ID\"}]"
}
```

`app_bindings` supplies the **complete** binding set. If present, Core validates
that set with `Binder.Validate`; invalid or incomplete explicit bindings must
fail, never fall back to an automatically selected key or LED. If absent, Core
retains its old auto-binding behavior. Explicit bindings are recommended for call
mode because request and acknowledgement must not select the same key.

| Mapping | `button-input` | `acknowledge-input` | `indicator` |
|---|---|---|---|
| One board | board A's **key3** stable entity ID | board A's **key2** stable entity ID | board A's LED-bank stable entity ID |
| Cross-board | workstation A's key stable entity ID | desk B's (or device C's) key stable entity ID | desk B's LED-bank stable entity ID |

The key names in this table are operator-facing mapping examples, not strings
hard-coded in the plugin. Copy the actual stable IDs, not display labels or
ambiguous device-local names. Cross-board use changes only the bindings; no
Driver, firmware, serial configuration or plugin hardware branch is required.
To omit the acknowledgement key or sound, omit its entry. To enable sound, add a
`sound` entry for a buzzer-capable entity and explicitly set `beep_on_press: true`.

## Management-console jobs

`Describe.Jobs` includes understandable titles and bounded `InputSchemaJSON`
for these actions. All call actions have **`ManualOnly: true`**; Core v0.2.15's
jobs GET response exposes `job_descriptors` for the operation UI. `bootstrap`
keeps its original automatic semantics. A scheduled invocation of a call action
is also rejected, and `request` requires explicit `confirm: true` as a second
guard.

### Clear the pending call (no arguments)

`POST /api/plugin-instances/{id}/jobs/acknowledge-pending/run`

```json
{
  "args_json": "{}",
  "idempotency_key": "desk-clear-001"
}
```

This is the primary duty-desk action. It acknowledges whichever call is
currently pending, so an operator never has to copy a `request_id` from a
record. It fails closed with `FAILED_PRECONDITION` when there is no pending
call, and it rejects any argument. The declarative UI renders it as a plain
button, which is why the label is just the action name.

### Request service

`POST /api/plugin-instances/{id}/jobs/request/run`

```json
{
  "args_json": "{\"confirm\":true,\"note\":\"Workstation 2 needs supplies\"}",
  "idempotency_key": "desk-request-001"
}
```

`confirm` must be boolean `true`; `note` is optional and limited to 256 Unicode
characters. Extra properties, nulls and malformed JSON are rejected. Decode the
returned `ResultJSON` / `result_json`: it contains `request_id`, `status`,
`created` and the full `request` with command outcomes. A pending call is returned
unchanged with `created: false`, even if another input/job originally created it.
An OK job response means the application handled the action, **not** that an LED
or buzzer command succeeded.

### Acknowledge a specific request

`POST /api/plugin-instances/{id}/jobs/acknowledge/run`

```json
{
  "args_json": "{\"request_id\":\"COPY_THE_EXACT_REQUEST_ID\"}",
  "idempotency_key": "desk-ack-001"
}
```

Copy the exact ID from the request result, current-process summary or domain
record. It must be a non-empty string of at most 128 characters without
whitespace. There is no "acknowledge latest" shortcut. An unknown ID is rejected;
repeating an acknowledged ID returns that old record without sending another off
command, even if a newer call is now pending. Reuse an idempotency key for retries
of the **same job and arguments**, not for a different request. Job deduplication
is scoped to the instance/job and current process.

Physical acknowledgement snapshots the pending ID before waiting behind another
transition. Known older source timestamps are rejected, as are duplicate
sequences and key bounce. A generic key event itself carries no business request
ID; exact operator-selected correlation is provided by the management job.

## Records and truthful device results

Core persists accepted `UpsertDomainRecord` effects as type `service_call` with
`record_id = request_id`. Records retain source, request/acknowledgement timestamps,
optional note, revision, and independent `indicator_on`, `indicator_off` and
optional `sound` result objects. Business `status` is only `pending` or
`acknowledged`; an output failure cannot acknowledge a pending call.

Each command has its own `command_id`, bound `entity_id`, `action`, desired mask
(for LED commands), deadline, result/error and observation timestamp. The public
protocol correlation is `RequestCompleted.RequestID = RequestCommand.IdempotencyKey`
(`{request_id}:on`, `:off` or `:sound`). Entity and action must also match. Only a
terminal completion changes the result to device-reported success/failure;
accepted/dispatched/running are not success. A late result updates its original
record only, never a newer request or another command.

| Command result `state` | Meaning |
|---|---|
| `awaiting_result` | command intent sent/being sent; no terminal device result observed |
| `succeeded` | Core delivered the matching terminal success result; not optical/physical acceptance evidence |
| `failed` / `timed_out` / `cancelled` | Core delivered that matching terminal outcome; error and result text are retained |
| `result_timeout` | no terminal result observed locally within 10 seconds; physical output is **unknown**, not assumed off or failed |
| `not_dispatched` | effect emission failed before this command was sent |
| `delivery_unknown` | sending this command failed ambiguously; it may or may not have reached Core/device |

The local result deadline does not fabricate a Core ACK. A later genuine
completion can replace `result_timeout`, while `result_timeout_at` preserves that
history. Failed effects are not silently retried as new commands. An active-stream
failure is returned to the job caller; changed records stay dirty for re-upsert
on a same-process stream reconnect or an explicit retry. The public SDK does not
acknowledge domain-store commits, so sending an upsert alone is not storage proof.

The read-only plugin summary and heartbeat expose `runtime_scope:
current_process_only`, `pending_request_id`, `request` and the latest `indicator`
result. Call mode deliberately does not publish the legacy speculative
`led_mask` field. In particular, `status: acknowledged` alongside an off
`result_timeout` means **business acknowledged, lamp-off unconfirmed**.

## Persistence and restart boundary

Domain records accepted by Core and declarative scheduler rows are persistent.
The public Application SDK currently offers **no domain-record read/recovery
operation**. This plugin cannot restore its pending-call pointer, command
correlations, key debounce or job-idempotency cache after a process restart.
It does not invent a recovered pending state, auto-acknowledge historical calls,
or turn off the indicator on startup. Request IDs are random to prevent a new
runtime from overwriting old records.

After restart, an empty current-process summary does **not** mean historical
pending calls were handled or the LED is off. Review the persistent record and
actual device/command state through Core before starting a new workflow. An
old ID unknown to this runtime is rejected rather than clearing any new request.
A new runtime cannot automatically reconcile its records or retry its lost
commands. Mode changes and rebinding are rejected while this runtime has a
pending call or is still waiting for a command result; unchanged bindings and
same-mode timezone/heartbeat updates are allowed.

## Development and acceptance

The repository pins the published public SDK v0.2.15. Verify its checksums
and run the full local gate:

```bash
go mod verify
go build ./...
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
gofmt -l .
python scripts/validate_manifest.py --self-test
python scripts/validate_manifest.py plugin.yaml --dir .
```

When co-developing against an unpublished SDK, use an ignored temporary modfile
with a local `replace` to a Core checkout that provides `JobDescriptor.ManualOnly`.
Run build/test/vet with `-modfile=.local/sdk-test.mod`; do not commit that replace
or manufacture release checksums. The tracked dependency remains v0.2.15.

Acceptance steps (hardware execution belongs to the integrator):

1. Omit `mode`: verify the existing walking light and its original tests still
   work, including silent defaults and the five-minute heartbeat.
2. Apply the explicit single-board mapping: key3 creates one pending record and
   one mask-255 command. Repeated presses create neither another call nor a beep.
   Confirm the terminal command result separately from the job/record creation.
3. Press key2, or run `acknowledge` with the exact ID: expect an acknowledged
   record and a mask-0 command. Claim lamp-off success only with a matching
   successful completion (and real-board observation for hardware acceptance).
4. Start a new call and retry the old acknowledgement/old completion: the new
   request must remain pending, with no new off command.
5. Exercise failed, cancelled and missing/late command results: the business call
   remains pending until explicit acknowledgement; missing results become
   `result_timeout`, not synthetic success. Check independent buzzer errors with
   sound explicitly enabled; the default stays silent.
6. Repeat using cross-device entity IDs. Inspect heartbeat records and confirm
   that only bootstrap, not request/acknowledge, is automatically dispatched.
7. Restart the plugin: verify old Core records remain visible, the summary states
   its current-process scope, and historical request IDs are not auto-restored.

The acknowledge schema uses a required string with length bounds, without
`pattern`, so the console can render a text field instead of falling back to JSON
input. `RunJob` still rejects empty, overlong and whitespace-containing request
IDs (including Unicode whitespace). Regression tests decode the descriptor to
check that `pattern` is absent and acknowledge an ID generated by the request job
after verifying invalid inputs return `INVALID_ARGUMENT` without effects.

Automated tests exercise the public Application Protocol wire, capability-only
single/cross-device mappings, job schemas, coalescing, stale acknowledgements,
concurrent jobs, effect-delivery errors and real timer behavior under Go's test
clock. They never open a serial port or substitute for real-board verification.

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
