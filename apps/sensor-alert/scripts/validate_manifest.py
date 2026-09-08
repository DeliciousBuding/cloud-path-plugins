#!/usr/bin/env python3
"""Stdlib-only gate for the Sensor Alert Application manifest.

JSON is accepted as a YAML subset by Core. The Go manifest test remains the
authoritative contract check; this script keeps the release gate independent of
the Go test runner.
"""
import argparse
import copy
import json
from pathlib import Path
import re
import sys

PLUGIN_ID = "io.github.deliciousbuding.cloud-path-app-sensor-alert"
ENTRYPOINT = "cloud-path-app-sensor-alert"
CORE = "github.com/DeliciousBuding/cloud-path"
REQUIREMENTS = [
    {"id": "temperature", "capability": "cloudpath.dev/capability/temperature@1", "cardinality": "zero-or-one"},
    {"id": "illuminance", "capability": "cloudpath.dev/capability/illuminance@1", "cardinality": "zero-or-one"},
    {"id": "contact", "capability": "cloudpath.dev/capability/hall@1", "cardinality": "zero-or-one"},
    {"id": "vibration", "capability": "cloudpath.dev/capability/vibration@1", "cardinality": "zero-or-one"},
    {"id": "alert-sound", "capability": "cloudpath.dev/capability/buzzer@1", "cardinality": "zero-or-one"},
    {"id": "alert-light", "capability": "cloudpath.dev/capability/led@1", "cardinality": "zero-or-one"},
]


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate key: {key}")
        result[key] = value
    return result


def parse(text):
    def invalid_constant(value):
        raise ValueError(f"non-JSON constant: {value}")
    return json.loads(text, object_pairs_hook=unique_object, parse_constant=invalid_constant)


def validate(value):
    expected = {
        "apiVersion": "plugins.cloudpath.dev/v1alpha1",
        "kind": "Application",
        "id": PLUGIN_ID,
        "version": "0.1.1",
        "protocol": 1,
        "entrypoint": ENTRYPOINT,
        "compatibility": {"core": ">=0.2.15 <0.3.0"},
        "permissions": {"hardware": [], "network": [], "filesystem": [], "secrets": []},
        "requirements": REQUIREMENTS,
    }
    errors = []
    if not isinstance(value, dict):
        return ["manifest must be an object"]
    for key, wanted in expected.items():
        if value.get(key) != wanted:
            errors.append(f"{key} must equal {wanted!r}")
    if type(value.get("protocol")) is not int:
        errors.append("protocol must be an integer")
    if set(value) != set(expected) | {"contributes"}:
        errors.append("missing or unexpected manifest keys")
    contributions = value.get("contributes")
    if not isinstance(contributions, dict) or set(contributions) != {"applications"}:
        errors.append("contributes must contain only applications")
    else:
        apps = contributions["applications"]
        if not isinstance(apps, list) or len(apps) != 1 or not isinstance(apps[0], dict):
            errors.append("exactly one application contribution is required")
        elif set(apps[0]) != {"id", "title"} or apps[0].get("id") != "sensor-alert" or not isinstance(apps[0].get("title"), str) or not apps[0]["title"].strip():
            errors.append("application contribution identity/title is invalid")
    return errors


def imports(source):
    for block in re.finditer(r'(?m)^import\s*(\([^)]*\)|[^\n]+)', source):
        yield from re.findall(r'"([^"\n]+)"', block.group(0))


def validate_tree(root, value):
    errors = validate(value)
    mirror = parse((root / "requirements.yaml").read_text(encoding="utf-8"))
    if mirror != {"requirements": REQUIREMENTS}:
        errors.append("requirements.yaml does not match the manifest")
    module = (root / "go.mod").read_text(encoding="utf-8")
    if not re.search(r'^module github.com/DeliciousBuding/cloud-path-app-sensor-alert$', module, re.M):
        errors.append("wrong Go module identity")
    if not re.search(r'^require github.com/DeliciousBuding/cloud-path v0\.2\.15$', module, re.M):
        errors.append("go.mod must pin the public Core v0.2.15 SDK")
    if re.search(r'^\s*(replace|exclude)\b', module, re.M):
        errors.append("go.mod must not contain development overrides")
    for source in root.rglob("*.go"):
        if any(part in {".git", ".local", ".worktrees", "vendor"} for part in source.relative_to(root).parts):
            continue
        for name in imports(source.read_text(encoding="utf-8")):
            if "/internal/" in name or name.endswith("/internal"):
                errors.append(f"non-public import in {source.relative_to(root)}: {name}")
            if name.startswith(CORE + "/") and not name.startswith(CORE + "/sdk/go/"):
                errors.append(f"non-SDK Core import in {source.relative_to(root)}: {name}")
    schema = parse((root / "config.schema.json").read_text(encoding="utf-8"))
    example = parse((root / "examples/app-config.json").read_text(encoding="utf-8"))
    if schema.get("type") != "object" or schema.get("additionalProperties") is not False:
        errors.append("config schema must be a closed object")
    if set(example) != set(schema.get("properties", {})):
        errors.append("config example and schema keys differ")
    return errors


def self_test(root):
    base = parse((root / "plugin.yaml").read_text(encoding="utf-8"))
    assert not validate(base)
    for key, bad in [
        ("version", "0.0.0"),
        ("protocol", True),
        ("compatibility", {"core": ">=0.2.14 <0.3.0"}),
        ("permissions", {"network": ["*"]}),
        ("requirements", REQUIREMENTS[:1]),
        ("contributes", {"applications": [{"id": "bad"}]}),
    ]:
        case = copy.deepcopy(base)
        case[key] = bad
        assert validate(case), f"accepted invalid {key}"
    for bad in ['{"id": 1, "id": 2}', '{"x": NaN}', '{} {}']:
        try:
            parse(bad)
        except ValueError:
            pass
        else:
            raise AssertionError("accepted malformed/ambiguous JSON")
    assert list(imports('package sample\nimport "x/internal/y"')) == ["x/internal/y"]
    print("manifest validator self-test OK")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("manifest", nargs="?", default="plugin.yaml")
    parser.add_argument("--dir", default=".")
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    root = Path(args.dir).resolve()
    try:
        if args.self_test:
            self_test(root)
            return 0
        value = parse(Path(args.manifest).read_text(encoding="utf-8"))
        errors = validate_tree(root, value)
    except (OSError, ValueError, AssertionError, KeyError, TypeError) as exc:
        print(f"manifest: {exc}", file=sys.stderr)
        return 1
    for error in errors:
        print(f"manifest: {error}", file=sys.stderr)
    if errors:
        return 1
    print("plugin manifest OK; requirements, SDK pin, zero permissions and public imports verified")
    return 0


if __name__ == "__main__":
    sys.exit(main())
