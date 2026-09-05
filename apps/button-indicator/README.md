# cloud-path-app-button-indicator

Button Indicator is the second CloudPath **Application plugin** and the
Milestone D3 reference for the platform north star:

> Adding a business changes neither the Driver nor the Core.

It binds three generic capabilities and contains zero knowledge of any
hardware, Driver id, port or vendor field:

| Requirement | Capability | Cardinality | Purpose |
|---|---|---|---|
| `button-input` | `cloudpath.dev/capability/key@1` | one-or-more | input |
| `indicator` | `cloudpath.dev/capability/led@1` | one | walking-light output |
| `sound` | `cloudpath.dev/capability/buzzer@1` | zero-or-one | optional press beep |

## Behavior

- **Key press** on a bound button entity advances a walking light by one LED
  (`mask = 1 << (count % 8)`) through a `request_command` effect to the bound
  indicator, and upserts a `press` domain record.
- **Declarative heartbeat**: a `schedule_job` effect (emitted idempotently by
  the `bootstrap` job on config changes) registers a cron task in the Core
  Durable Scheduler. Every cron tick dispatches the `indicator-heartbeat` job,
  which upserts a `heartbeat` domain record. This is the real-board gate for
  the scheduler primitive: declaration → cron dispatch → domain record, all
  restart-safe with the `skip` missed-run policy.

## Configuration (`app_config`)

```json
{
  "timezone": "Asia/Shanghai",
  "heartbeat_cron": "*/5 * * * *",
  "beep_on_press": false
}
```

All fields optional. Defaults: UTC, `*/5 * * * *`, no beep.

## Development

```bash
go build ./...
go vet ./...
go test ./... -count=1
gofmt -l .
python scripts/validate_manifest.py plugin.yaml --dir .
```

The plugin is published through the repository release workflow; installs are
digest-verified against `checksums.txt` (see the CloudPath plugin docs).
