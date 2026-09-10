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


UI = {
    "apiVersion": 1,
    "navigation": {
        "title": "环境告警",
        "icon": "bell-ring",
        "order": 40,
        "route": "sensor-alert",
        "visibility": "always"
    },
    "pages": [
        {
            "id": "home",
            "title": "环境与安防告警",
            "description": "监测温度、光照、接触和振动，达到设定条件时发出声光提醒，并记录最近一次告警。",
            "sections": [
                {
                    "type": "status",
                    "title": "运行状态",
                    "description": "查看告警功能是否已经启用，以及设备和平台是否连接正常。",
                    "emptyText": "暂无运行状态信息。",
                    "source": "instance"
                },
                {
                    "type": "metrics",
                    "title": "当前告警",
                    "description": "显示最近一次告警的状态、传感器、当前值和触发时间。",
                    "emptyText": "还没有告警。若尚未布防，请先在「告警开关」中布防。",
                    "source": "records",
                    "recordType": "alert",
                    "fields": [
                        {
                            "key": "state",
                            "label": "告警状态",
                            "values": {
                                "armed": "已启用",
                                "triggered": "已触发",
                                "recovered": "已恢复",
                                "disarmed": "已停用"
                            },
                            "hideWhenEmpty": True
                        },
                        {
                            "key": "sensor",
                            "label": "传感器",
                            "values": {
                                "temperature": "温度",
                                "illuminance": "光照",
                                "contact": "接触",
                                "vibration": "振动"
                            },
                            "hideWhenEmpty": True
                        },
                        {
                            "key": "value",
                            "label": "当前值",
                            "format": "number",
                            "hideWhenEmpty": True
                        },
                        {
                            "key": "triggered_at",
                            "label": "触发时间",
                            "format": "time",
                            "hideWhenEmpty": True
                        }
                    ]
                },
                {
                    "type": "actions",
                    "title": "告警开关",
                    "description": "配置默认会自动布防；也可手动点击“布防告警”启用，或点击“撤防告警”停用。",
                    "emptyText": "暂无可执行的告警操作。",
                    "source": "manual-jobs"
                },
                {
                    "type": "records",
                    "title": "最近告警",
                    "description": "查看最近一次告警发生在哪个传感器、当时是什么状态、数值和触发阈值。",
                    "emptyText": "还没有告警。若尚未布防，请先点击“布防告警”开始监测；已布防但还没有触发时会显示在这里。",
                    "source": "records",
                    "recordType": "alert",
                    "presentation": "timeline",
                    "fields": [
                        {
                            "key": "sensor",
                            "label": "传感器",
                            "values": {
                                "temperature": "温度",
                                "illuminance": "光照",
                                "contact": "接触",
                                "vibration": "振动"
                            },
                            "hideWhenEmpty": True
                        },
                        {
                            "key": "state",
                            "label": "告警状态",
                            "values": {
                                "armed": "已启用",
                                "triggered": "已触发",
                                "recovered": "已恢复",
                                "disarmed": "已停用"
                            },
                            "hideWhenEmpty": True
                        },
                        {
                            "key": "value",
                            "label": "当前值",
                            "format": "number",
                            "hideWhenEmpty": True
                        },
                        {
                            "key": "threshold",
                            "label": "触发阈值",
                            "format": "number",
                            "hideWhenEmpty": True
                        },
                        {
                            "key": "triggered_at",
                            "label": "触发时间",
                            "format": "time",
                            "hideWhenEmpty": True
                        }
                    ]
                },
                {
                    "type": "form",
                    "title": "告警设置",
                    "description": "设置触发告警的条件，以及触发后如何提醒。",
                    "emptyText": "暂无可配置项。",
                    "source": "config",
                    "fields": [
                        {
                            "key": "app_config.auto_arm",
                            "label": "自动布防",
                            "type": "boolean",
                            "description": "打开后，配置生效（含插件重启后）会自动布防并开始监测；关闭后需要手动点击“布防告警”。",
                            "default": True
                        },
                        {
                            "key": "app_config.temperature_min",
                            "label": "最低温度（传感器单位）",
                            "type": "number",
                            "description": "低于这个温度时触发告警；按传感器原始单位比较，不做单位换算。",
                            "default": 18
                        },
                        {
                            "key": "app_config.temperature_max",
                            "label": "最高温度（传感器单位）",
                            "type": "number",
                            "description": "高于这个温度时触发告警；按传感器原始单位比较，不做单位换算。",
                            "default": 28
                        },
                        {
                            "key": "app_config.light_min",
                            "label": "最低光照（原始读数）",
                            "type": "number",
                            "description": "低于这个数值时触发告警；数值为传感器原始读数（未标定），留空表示不检查。"
                        },
                        {
                            "key": "app_config.light_max",
                            "label": "最高光照（原始读数）",
                            "type": "number",
                            "description": "高于这个数值时触发告警；数值为传感器原始读数（未标定），留空表示不检查。"
                        },
                        {
                            "key": "app_config.contact_enabled",
                            "label": "接触告警",
                            "type": "boolean",
                            "description": "打开后，门、盖或窗被打开时触发告警。",
                            "default": False
                        },
                        {
                            "key": "app_config.vibration_enabled",
                            "label": "振动告警",
                            "type": "boolean",
                            "description": "打开后，检测到明显振动时触发告警。",
                            "default": False
                        },
                        {
                            "key": "app_config.cooldown_s",
                            "label": "重复提醒间隔（秒）",
                            "type": "integer",
                            "description": "同一问题再次触发前至少等待多久；填 0 表示不限制。",
                            "minimum": 0,
                            "maximum": 86400,
                            "default": 60
                        },
                        {
                            "key": "app_config.silent",
                            "label": "静音提醒",
                            "type": "boolean",
                            "description": "打开后只亮指示灯，不发出提示音。",
                            "default": False
                        }
                    ]
                }
            ]
        }
    ]
}

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
        "version": "0.2.1",
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
        elif not {"id", "title", "ui"}.issubset(apps[0]) or apps[0].get("id") != "sensor-alert" or not isinstance(apps[0].get("title"), str) or not apps[0].get("title", "").strip():
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
    readme = (root / "README.md").read_text(encoding="utf-8")
    version = value.get("version")
    if not isinstance(version, str) or f"版本：`{version}`" not in readme:
        errors.append("README.md version does not match the manifest")
    core = value.get("compatibility", {}).get("core")
    if not isinstance(core, str) or f"Core `{core}`" not in readme:
        errors.append("README.md Core compatibility does not match the manifest")
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
