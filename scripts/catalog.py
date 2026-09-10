#!/usr/bin/env python3
"""Validate and query the CloudPath plugin catalog.

The catalog intentionally uses a small YAML subset so this tool remains
stdlib-only. Core consumes the same file with a full YAML parser.
"""
from __future__ import annotations

import argparse
import json
import re
import sys
from dataclasses import dataclass, asdict
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
CATALOG = ROOT / "plugins.yaml"
VALID_KINDS = {"Driver", "Application", "Connector"}


@dataclass(frozen=True)
class Plugin:
    id: str
    slug: str
    kind: str
    path: str
    tag_prefix: str
    asset: str
    version: str
    entrypoint: str

    def as_json(self) -> dict[str, str]:
        return {
            "id": self.id,
            "slug": self.slug,
            "kind": self.kind,
            "path": self.path,
            "tagPrefix": self.tag_prefix,
            "asset": self.asset,
            "version": self.version,
            "entrypoint": self.entrypoint,
        }


def _scalar(raw: str) -> str:
    value = raw.strip()
    if len(value) >= 2 and value[0] == value[-1] and value[0] in {"'", '"'}:
        return value[1:-1]
    return value


def _parse_catalog(path: Path) -> dict:
    api_version = kind = None
    entries: list[dict] = []
    current: dict | None = None
    in_plugins = False
    for number, raw in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        line = raw.rstrip()
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        indent = len(line) - len(line.lstrip(" "))
        text = line.strip()
        if indent == 0:
            key, sep, value = text.partition(":")
            if not sep:
                raise ValueError(f"{path}:{number}: expected key: value")
            if key == "apiVersion":
                api_version = _scalar(value)
            elif key == "kind":
                kind = _scalar(value)
            elif key == "plugins" and not value.strip():
                in_plugins = True
            else:
                raise ValueError(f"{path}:{number}: unsupported root key {key!r}")
            continue
        if not in_plugins:
            raise ValueError(f"{path}:{number}: content outside plugins list")
        if indent == 2 and text.startswith("- "):
            if current:
                entries.append(current)
            current = {}
            text = text[2:].strip()
            if not text:
                continue
        elif indent != 4 or current is None:
            raise ValueError(f"{path}:{number}: unsupported catalog indentation")
        key, sep, value = text.partition(":")
        if not sep:
            raise ValueError(f"{path}:{number}: expected key: value")
        key = key.strip()
        if key in current:
            raise ValueError(f"{path}:{number}: duplicate key {key!r}")
        current[key] = _scalar(value)
    if current:
        entries.append(current)
    if api_version != "plugins.cloudpath.dev/v1alpha1":
        raise ValueError(f"{path}: unsupported apiVersion {api_version!r}")
    if kind != "PluginCatalog":
        raise ValueError(f"{path}: unsupported kind {kind!r}")
    if not entries:
        raise ValueError(f"{path}: plugins list is empty")
    return {"apiVersion": api_version, "kind": kind, "plugins": entries}


def _manifest_value(text: str, key: str) -> str:
    patterns = [
        rf'(?m)^\s*{re.escape(key)}:\s*["\']?([^"\'\s#]+)["\']?\s*$',
        rf'(?m)^\s*"{re.escape(key)}"\s*:\s*"([^"]+)"\s*,?\s*$',
    ]
    for pattern in patterns:
        match = re.search(pattern, text)
        if match:
            return match.group(1).strip()
    raise ValueError(f"manifest missing {key}")


def _module_path(path: Path) -> str:
    for line in path.read_text(encoding="utf-8").splitlines():
        if line.startswith("module "):
            return line.split(None, 1)[1].strip()
    raise ValueError(f"{path}: missing module declaration")


def load_plugins() -> list[Plugin]:
    data = _parse_catalog(CATALOG)
    required = {"id", "slug", "kind", "path", "tagPrefix", "asset"}
    seen: set[tuple[str, str]] = set()
    plugins: list[Plugin] = []
    for index, raw in enumerate(data["plugins"], 1):
        missing = required - raw.keys()
        if missing:
            raise ValueError(f"plugins[{index}] missing fields: {', '.join(sorted(missing))}")
        values = {
            "id": str(raw["id"]).strip(),
            "slug": str(raw["slug"]).strip(),
            "kind": str(raw["kind"]).strip(),
            "path": str(raw["path"]).strip(),
            "tag_prefix": str(raw["tagPrefix"]).strip(),
            "asset": str(raw["asset"]).strip(),
        }
        if not values["id"] or not values["slug"] or not values["asset"]:
            raise ValueError(f"plugins[{index}] has an empty id/slug/asset")
        if values["kind"] not in VALID_KINDS:
            raise ValueError(f"plugins[{index}] invalid kind {values['kind']!r}")
        rel = Path(values["path"])
        if rel.is_absolute() or ".." in rel.parts or not rel.parts or rel.parts[0] not in {"apps", "drivers"}:
            raise ValueError(f"plugins[{index}] unsafe path {values['path']!r}")
        if values["tag_prefix"] != values["path"]:
            raise ValueError(f"plugins[{index}] tagPrefix must equal path")
        plugin_dir = ROOT / rel
        if not plugin_dir.is_dir():
            raise ValueError(f"plugins[{index}] directory does not exist: {values['path']}")
        manifest = plugin_dir / "plugin.yaml"
        if not manifest.is_file():
            raise ValueError(f"plugins[{index}] missing {manifest.relative_to(ROOT)}")
        for forbidden in (".github", ".gitattributes", ".gitignore", "LICENSE", "NOTICE"):
            if (plugin_dir / forbidden).exists():
                raise ValueError(f"plugins[{index}] contains module-local {forbidden}")
        manifest_text = manifest.read_text(encoding="utf-8")
        manifest_id = _manifest_value(manifest_text, "id")
        manifest_kind = _manifest_value(manifest_text, "kind")
        version = _manifest_value(manifest_text, "version")
        entrypoint = _manifest_value(manifest_text, "entrypoint")
        license_id = _manifest_value(manifest_text, "license")
        if manifest_id != values["id"]:
            raise ValueError(f"plugins[{index}] id mismatch: catalog={values['id']} manifest={manifest_id}")
        if manifest_kind != values["kind"]:
            raise ValueError(f"plugins[{index}] kind mismatch: catalog={values['kind']} manifest={manifest_kind}")
        if license_id != "Apache-2.0":
            raise ValueError(f"plugins[{index}] first-party license must be Apache-2.0")
        if entrypoint != values["asset"]:
            raise ValueError(f"plugins[{index}] asset must match manifest entrypoint")
        expected_module = f"github.com/DeliciousBuding/cloud-path-plugins/{values['path']}"
        module = _module_path(plugin_dir / "go.mod")
        if module != expected_module:
            raise ValueError(f"plugins[{index}] module mismatch: got {module!r}, want {expected_module!r}")
        key = (values["id"], values["path"])
        if key in seen:
            raise ValueError(f"plugins[{index}] duplicate id/path {key}")
        seen.add(key)
        plugins.append(Plugin(**values, version=version, entrypoint=entrypoint))
    return plugins


def find_by_tag(tag: str) -> tuple[Plugin, str]:
    for plugin in load_plugins():
        prefix = plugin.tag_prefix + "/v"
        if tag.startswith(prefix):
            version = tag[len(prefix):]
            if version != plugin.version:
                raise ValueError(f"tag version {version!r} != manifest version {plugin.version!r}")
            return plugin, version
    raise ValueError(f"unknown plugin tag {tag!r}")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--check", action="store_true")
    parser.add_argument("--list", action="store_true")
    parser.add_argument("--matrix", action="store_true")
    parser.add_argument("--resolve-tag")
    args = parser.parse_args()
    try:
        if args.resolve_tag:
            plugin, version = find_by_tag(args.resolve_tag)
            print(json.dumps({**plugin.as_json(), "version": version}, sort_keys=True))
            return 0
        plugins = load_plugins()
        if args.matrix:
            print(json.dumps([p.path for p in plugins]))
        elif args.list or args.check:
            for plugin in plugins:
                print(f"{plugin.slug}\t{plugin.kind}\t{plugin.version}\t{plugin.path}")
            if args.check:
                print(f"catalog: OK ({len(plugins)} active plugins)")
        else:
            parser.print_help()
        return 0
    except (OSError, ValueError) as exc:
        print(f"catalog: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
