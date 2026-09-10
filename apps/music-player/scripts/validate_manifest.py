#!/usr/bin/env python3
"""Stdlib-only gate for the Music Player Application manifest.

The manifest is JSON, which is a YAML subset accepted by CloudPath Core. This
validator pins the public identity, capability requirements, SDK version and
import boundary without adding a YAML dependency to the repository.
"""
import argparse
import copy
import json
from pathlib import Path
import re
import sys

PLUGIN_ID = "io.github.deliciousbuding.cloud-path-app-music-player"
ENTRYPOINT = "cloud-path-app-music-player"
CORE = "github.com/DeliciousBuding/cloud-path"
REQUIREMENTS = [
    {"id": "sound", "capability": "cloudpath.dev/capability/buzzer@1", "cardinality": "one"},
    {"id": "local-display", "capability": "cloudpath.dev/capability/display-text@1", "cardinality": "zero-or-one"},
    {"id": "indicator", "capability": "cloudpath.dev/capability/led@1", "cardinality": "zero-or-one"},
]


UI = {'apiVersion': 1,
 'navigation': {'title': '音乐播放器', 'icon': 'music', 'order': 50, 'route': 'music', 'visibility': 'always'},
 'pages': [{'id': 'home',
            'title': '音乐播放器',
            'description': '可以远程选择内置歌曲并播放，也可以播放一个指定音符。',
            'sections': [{'type': 'status',
                          'source': 'instance',
                          'title': '播放设备',
                          'description': '查看音乐播放器当前是否可用。',
                          'emptyText': '暂无设备状态。'},
                         {'type': 'metrics',
                          'source': 'records',
                          'recordType': 'music_session',
                          'title': '最近一次播放',
                          'description': '显示最近一次播放的歌曲、状态、已完成音符和发起时间。',
                          'emptyText': '还没有播放记录。',
                          'fields': [{'key': 'song',
                                      'label': '歌曲',
                                      'values': {'little-star': '小星星',
                                                 'birthday': '生日歌',
                                                 'ode-to-joy': '欢乐颂',
                                                 'custom': '自定义曲目'},
                                      'hideWhenEmpty': True},
                                     {'key': 'status',
                                      'label': '播放状态',
                                      'values': {'idle': '未开始',
                                                 'queued': '等待播放',
                                                 'playing': '播放中',
                                                 'completed': '播放完成',
                                                 'failed': '播放失败'}},
                                     {'key': 'completed_notes',
                                      'label': '已完成音符',
                                      'unit': '个',
                                      'precision': 0,
                                      'hideWhenEmpty': True},
                                     {'key': 'queued_at', 'label': '发起时间', 'format': 'time', 'hideWhenEmpty': True}]},
                         {'type': 'actions',
                          'source': 'manual-jobs',
                          'title': '播放控制',
                          'description': '点一首内置曲目即可播放，也可以播放单个音符。',
                          'emptyText': '暂无可用的播放操作。'},
                         {'type': 'records',
                          'source': 'records',
                          'recordType': 'music_session',
                          'presentation': 'timeline',
                          'title': '播放记录',
                          'description': '按时间查看最近的播放情况。',
                          'emptyText': '还没有播放记录。',
                          'fields': [{'key': 'title', 'label': '标题'},
                                     {'key': 'summary', 'label': '播放概况'},
                                     {'key': 'song',
                                      'label': '歌曲',
                                      'values': {'little-star': '小星星',
                                                 'birthday': '生日歌',
                                                 'ode-to-joy': '欢乐颂',
                                                 'custom': '自定义曲目'}},
                                     {'key': 'status',
                                      'label': '状态',
                                      'values': {'idle': '未开始',
                                                 'queued': '等待播放',
                                                 'playing': '播放中',
                                                 'completed': '播放完成',
                                                 'failed': '播放失败'}},
                                     {'key': 'queued_at',
                                      'label': '发起时间',
                                      'format': 'time',
                                      'hideWhenEmpty': True}]}]}]}

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



def strip_i18n(value):
    if isinstance(value, dict):
        return {key: strip_i18n(child) for key, child in value.items() if key not in {"i18n", "valuesI18n"}}
    if isinstance(value, list):
        return [strip_i18n(child) for child in value]
    return value

def validate(value):
    expected = {
        "apiVersion": "plugins.cloudpath.dev/v1alpha1",
        "kind": "Application",
        "id": PLUGIN_ID,
        "version": "0.3.1",
        "protocol": 1,
        "entrypoint": ENTRYPOINT,
        "compatibility": {"core": ">=0.2.29 <0.3.0"},
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
        elif not {"id", "title", "ui"}.issubset(apps[0]) or apps[0].get("id") != "music-player" or not isinstance(apps[0].get("title"), str) or not apps[0].get("title", "").strip():
            errors.append("application contribution identity/title is invalid")
        elif strip_i18n(apps[0].get("ui")) != UI:
            errors.append("application UI contribution is invalid")
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
    if not re.search(r'^module github.com/DeliciousBuding/cloud-path-app-music-player$', module, re.M):
        errors.append("wrong Go module identity")
    if not re.search(r'^require github.com/DeliciousBuding/cloud-path v0\.2\.15$', module, re.M):
        errors.append("go.mod must pin the public Core v0.2.15 SDK")
    if re.search(r'^\s*(replace|exclude)\b', module, re.M):
        errors.append("go.mod must not contain development overrides")
    for source in root.rglob("*.go"):
        relative = source.relative_to(root)
        if any(part in {".git", ".local", ".worktrees", "vendor"} for part in relative.parts):
            continue
        for name in imports(source.read_text(encoding="utf-8")):
            if "/internal/" in name or name.endswith("/internal"):
                errors.append(f"non-public import in {relative}: {name}")
            if name.startswith(CORE + "/") and not name.startswith(CORE + "/sdk/go/"):
                errors.append(f"non-SDK Core import in {relative}: {name}")
    readme = (root / "README.md").read_text(encoding="utf-8")
    version = value.get("version")
    if not isinstance(version, str) or f"Version **{version}**" not in readme:
        errors.append("README.md version does not match the manifest")
    changelog_path = root / "CHANGELOG.md"
    if not changelog_path.is_file() or not isinstance(version, str) or f"## v{version} " not in changelog_path.read_text(encoding="utf-8"):
        errors.append("CHANGELOG.md is missing the current release entry")
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
    bad_ui = copy.deepcopy(base)
    bad_ui["contributes"]["applications"][0]["ui"]["navigation"]["route"] = "bad/route"
    assert validate(bad_ui), "accepted invalid UI route"
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
        manifest_path = Path(args.manifest)
        if not manifest_path.is_absolute():
            manifest_path = root / manifest_path
        value = parse(manifest_path.read_text(encoding="utf-8"))
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
