# Contributing

CloudPath Plugins is a multi-module Go monorepo. Keep each plugin independently buildable and releasable; changes to shared release tooling must preserve that property.

Before submitting a change:

```bash
python scripts/catalog.py --check
cd apps/<plugin> && gofmt -l . && go test ./... && go vet ./... && go build ./...
```

Release tags follow `<module-path>/v<semver>`. Do not create a repository-wide version tag. Update `plugin.yaml`, the implementation version constant, README references, and the relevant changelog together.
