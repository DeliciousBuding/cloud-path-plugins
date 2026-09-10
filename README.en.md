# CloudPath Plugins

CloudPath Plugins is the source-of-truth monorepo for the official CloudPath
Driver and Application plugins. Each plugin keeps its own Go module, manifest,
semantic version, and GitHub Release; `plugins.yaml` is the monorepo discovery
catalog.

> Migration note: this repository is the canonical source for official
> plugins. The legacy `cloud-path-driver-stcb` and `cloud-path-app-*`
> repositories are archived after migration and retain historical tags and
> releases only; active development, issues, catalog discovery, and releases
> live here.

## Active plugins

| Slug | Kind | Version | Source |
|---|---|---:|---|
| `stcb` | Driver | 0.2.14 | `drivers/stcb/` |
| `scheduled-compartment` | Application | 0.3.3 | `apps/scheduled-compartment/` |
| `button-indicator` | Application | 0.2.3 | `apps/button-indicator/` |
| `music-player` | Application | 0.3.3 | `apps/music-player/` |
| `sensor-alert` | Application | 0.2.3 | `apps/sensor-alert/` |

## Install

Core `v0.2.43+` can resolve a plugin from this repository's catalog:

```bash
cloudpath plugin install DeliciousBuding/cloud-path-plugins \
  --plugin scheduled-compartment \
  --digest sha256:<release-digest> \
  --yes
```

Single-plugin repositories remain supported. Always use an independent digest,
the curated registry, or build attestation before enabling a production plugin.

## Develop

```bash
go work sync
cd apps/scheduled-compartment
gofmt -l .
go test ./... -count=1
go vet ./...
go build ./...
```

Catalog and release helpers are stdlib-only:

```bash
python scripts/catalog.py --check
python scripts/catalog.py --list
python scripts/release.py --tag apps/music-player/v0.3.3 --out dist
```

## Releases

Service each plugin independently:

```text
drivers/stcb/v0.2.14
apps/scheduled-compartment/v0.3.3
apps/button-indicator/v0.2.3
apps/music-player/v0.3.3
apps/sensor-alert/v0.2.3
```

The release workflow verifies the tag, catalog, module path, manifest, and
version before building six platform binaries and checksums.

## Archive

The `archive/` tree preserves retired plugins for historical reference. Archived
plugins are excluded from discovery, CI, and release automation.

## License

First-party code is licensed under the Apache License 2.0. See `LICENSE`,
`NOTICE`, and `THIRD_PARTY_NOTICES.md`.
