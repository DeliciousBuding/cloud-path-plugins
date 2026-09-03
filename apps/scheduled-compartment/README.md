# cloud-path-app-scheduled-compartment

> Standalone repository for the CloudPath reference **Application** plugin
> `scheduled-compartment`. Generated from the CloudPath core monorepo by
> `deploy/split/split_app_plugin.py`; edit the upstream source, then regenerate.

## Module and SDK dependency

- Module path: `github.com/DeliciousBuding/cloud-path-app-scheduled-compartment`
- Depends only on the public CloudPath SDK (`github.com/DeliciousBuding/cloud-path/sdk/...`) — no core
  `internal/` package is imported, and the generator fails hard if one appears.
- `go.mod` requires the published core module (`require github.com/DeliciousBuding/cloud-path <version>`).
  That is the shippable form: a clean checkout builds with no local assumptions
  once the core tag exists.
- For local development against an unpublished core checkout, add a temporary
  `replace github.com/DeliciousBuding/cloud-path => ../cloud-path` (the generator can write it with
  `--core-path`, in which case it also drops a `LOCAL_REPLACE.txt` marker).
  **Never publish a tree that still carries that replace line.**

---

`cloud-path-app-scheduled-compartment` is a **device-agnostic** reference
Application plugin for CloudPath. It manages a set of compartments against a
daily schedule: when a schedule window starts it emits a reminder, observes
contact opened/closed events for each compartment, records a completed or
missed outcome per window, and keeps everything idempotent.

The application does **not** depend on any Driver ID, port or vendor field. It
is expressed purely in terms of standard Capability requirements and stable
`entity_id` bindings, so the same application can be deployed against any set
of entities that expose those capabilities.

Only the following domain terms are used: **schedule**, **window**,
**compartment**, **opened**, **completed**, **missed** and **reminder**. No
industry-specific semantics are hard-coded.

## Dependencies

- **Go** — the exact language version is declared in this repository's
  `go.mod` (kept in sync with the core monorepo by the generator).
- **Only the public CloudPath SDK and protocol schema.** The code imports
  these packages from `github.com/DeliciousBuding/cloud-path` and nothing else
  from the Core repository:

  | Import path | Purpose |
  |---|---|
  | `sdk/go/cloudpath/v1/application` | Application Protocol v1 types and RPC |
  | `sdk/go/cloudpath/v1/status` | Status codes |
  | `sdk/go/pluginmain` | Host-injected launch identity and handshake |
  | `sdk/go/rpc` | RPC server |
  | `sdk/go/transport` | Host-provided transport |

  There are **no imports from `internal/`** of the Core repository. Re-check
  the boundary from this directory with:

  ```bash
  grep -R "cloud-path/internal" --include="*.go" .
  # expected: no output
  ```

- The manifest requests **no hardware, network, filesystem or secret
  permissions**.

## Required Capabilities

| Requirement ID | Capability | Cardinality | Purpose |
|---|---|---|---|
| `reminder-output` | `cloudpath.dev/capability/alarm@1` | one | Emit scheduled reminders |
| `compartments` | `cloudpath.dev/capability/contact@1` | one-or-more, minimum 3 | Represent and monitor compartments |
| `local-display` | `cloudpath.dev/capability/display-text@1` | zero-or-one | Optional local status text |

The same declarations live in `requirements.yaml` (human review) and
`plugin.yaml` (machine manifest). `Describe` returns the equivalent
`ApplicationDescriptor`, so the runtime and the manifest cannot drift; the
`manifest_test.go` suite enforces that all three copies stay identical.

## Instance Configuration

The instance config is a bounded JSON object. It is validated on
`ConfigureInstance`; a missing field, a duplicate compartment, an unknown
window compartment or an invalid time is rejected with a non-OK status.

```json
{
  "timezone": "Asia/Shanghai",
  "compartments": [
    {"id": "c1", "name": "Compartment 1"},
    {"id": "c2", "name": "Compartment 2"},
    {"id": "c3", "name": "Compartment 3"}
  ],
  "schedule": [
    {"id": "w-morning", "compartment": "c1", "start": "08:00", "end": "08:30"}
  ]
}
```

Field rules:

- `timezone`: required, a valid IANA name (for example `Asia/Shanghai`).
- `compartments`: required, at least one, each with a unique non-empty `id`.
- `schedule`: required, at least one window. Each window has a unique non-empty
  `id`, a `compartment` that references a configured compartment, and `start` /
  `end` in 24-hour `HH:MM`. `end` must be strictly after `start`.

`ScheduleTick` events carry the concrete runtime window (with RFC3339 `start` /
`end` timestamps). The app validates that the referenced compartment exists in
the instance config before starting the window.

## Binding

An application instance is bound to Capabilities rather than to a device or
Driver. `ValidateBinding` enforces the declared cardinalities and rejects any
requirement id that this application does not declare (which structurally rules
out Driver coupling).

| Requirement ID | Candidate Entity |
|---|---|
| `reminder-output` | one alarm-capable Entity |
| `compartments[0..n]` | contact-capable Entities, at least 3 |
| `local-display` | an optional display-text-capable Entity |

Bindings persist stable `entity_id` values. Reconnects, Edge restarts and
Driver restarts must not change a binding. A valid binding is stored so that
the reminder is routed to the bound alarm entity and contact events are mapped
back to their compartment.

## Behavior

The runtime is a process-based `ApplicationService` (Application Protocol v1):

1. **Window start** — on a `ScheduleTick`, the app opens a window, emits a
   `UpsertDomainRecord` (`window`, `state=opened`), a `RequestCommand` to the
   bound alarm entity, and a `ScheduleTask` so Core can later trigger the
   `window-check` job.
2. **Completion** — on a contact `opened`/`closed` event for the compartment
   while its window is open, the app marks the window `completed` and cancels
   the `window-check` task.
3. **Missed** — the `window-check` job scans open windows against the clock and
   records a `window` record with `state=missed`, cancels the task and emits a
   notification.
4. **Idempotency** — duplicate events (same sequence or same window id) and
   repeated jobs (same `IdempotencyKey`) do not emit duplicate effects.

Effects are limited to the Core-approved closed set: `UpsertDomainRecord`,
`DeleteDomainRecord`, `RequestCommand`, `ScheduleTask`,
`CancelScheduledTask` and `SendNotification`. The app never produces arbitrary
SQL, shell commands, file/network effects or global credential requests.

The `HandleRequest` subroute is read-only and returns a bounded JSON summary of
the instance config and window state.

## Build

From a standalone checkout of this repository:

```bash
# Application library, tests and entrypoint command
go build ./...
go vet ./...

# Build just the entrypoint binary
go build -o cloud-path-app-scheduled-compartment ./cmd/cloud-path-app-scheduled-compartment
```

The entrypoint binary is `cloud-path-app-scheduled-compartment`, matching the
`entrypoint` field in `plugin.yaml`.

This repository is generated from the CloudPath core monorepo
(`github.com/DeliciousBuding/cloud-path`, incubation path
`examples/scheduled-compartment`) by `deploy/split/split_app_plugin.py`. The
package layout here is the standalone one: the application library lives at the
module root and the entrypoint under `cmd/`.

## Test

```bash
go test ./... -count=1
go test ./... -count=20   # idempotency / flake soak
```

The suite covers the descriptor requirements, config/binding validation, the
window reminder effect, contact-driven completion, missed-window recording,
duplicate-event idempotency, rejection of driver coupling, invalid config,
graceful shutdown, and manifest identity / requirements drift.

Repository-level gates in this checkout:

```bash
gofmt -l .                                  # expected: no output
python scripts/validate_manifest.py --self-test
python scripts/validate_manifest.py plugin.yaml --dir .
```

The full binary-to-Host end-to-end harness lives in the CloudPath core
repository (`testing/plugin-harness`) because it needs the Core Plugin Host;
it is not duplicated here.

## Run as an Application Plugin

`cmd/cloud-path-app-scheduled-compartment` is an install-style, process-based
Application plugin. It is launched by the CloudPath Plugin Host, never with
manual flags: the Host injects the launch identity and a loopback endpoint
through `CLOUDPATH_*` environment variables (the contract is defined by the
public `pluginmain` package), the process prints the single `CP1` handshake
line, dials back and serves Application Protocol v1 over that authenticated
transport. Started outside a Host it exits immediately with a missing
environment error.

To attach it to a Host:

1. Build the entrypoint binary (see Build).
2. Publish `plugin.yaml` and the entrypoint binary as a release asset together
   with its published sha256 digest.
3. Install and enable the instance:

   ```bash
   cloudpath plugin install <repository-url-or-id> --digest sha256:<hex> --yes
   cloudpath plugin enable io.github.deliciousbuding.cloud-path-app-scheduled-compartment
   ```

4. Start the Host, which supervises the plugin process and serves the
   protocol:

   ```bash
   cloudpath plugin host
   ```

The runtime then delivers the instance configuration through
`ConfigureInstance` and entity bindings through `ValidateBinding` (see
Instance Configuration and Binding).

## Disable and remove

```bash
cloudpath plugin disable io.github.deliciousbuding.cloud-path-app-scheduled-compartment
cloudpath plugin remove  io.github.deliciousbuding.cloud-path-app-scheduled-compartment
```

`remove` keeps the instance data; add `--purge` to delete it as well.

## Repository Layout

| Path | Purpose |
|---|---|
| `plugin.yaml` | Machine manifest (id, version, protocol, entrypoint, requirements, contributions) |
| `requirements.yaml` | Human-readable requirement mirror |
| `config.go` | Bounded instance config schema and validation |
| `service.go` | `ApplicationService` implementation and state machine |
| `service_test.go` | Conformance/behaviour tests over the real wire |
| `manifest_test.go` | Machine identity lock + manifest/requirements/descriptor drift tests |
| `cmd/cloud-path-app-scheduled-compartment/` | Executable entrypoint |

## Status

Implemented as a runnable reference application. It is a process-based plugin
that depends only on the public CloudPath SDK and schema.
