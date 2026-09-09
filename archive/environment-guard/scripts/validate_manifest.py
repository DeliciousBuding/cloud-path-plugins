#!/usr/bin/env python3
"""Stdlib-only gate for this component's JSON-compatible YAML manifests.

JSON is a YAML subset accepted by Core; using it here avoids an incomplete YAML
parser or a build-only dependency. Validate the complete component contract,
requirements mirror, SDK pin, and Go import boundaries. This is intentionally not
an implementation of arbitrary third-party manifests or all JSON Schema drafts.
"""
import argparse
import copy
import json
from pathlib import Path
import re
import sys

PLUGIN_ID = "io.github.deliciousbuding.cloud-path-app-environment-guard"
ENTRYPOINT = "cloud-path-app-environment-guard"
CORE = "github.com/DeliciousBuding/cloud-path"
REQUIREMENTS = [
    {"id": "temperature", "capability": "cloudpath.dev/capability/temperature@1", "cardinality": "one"},
    {"id": "illuminance", "capability": "cloudpath.dev/capability/illuminance@1", "cardinality": "one"},
]


UI = {
    "apiVersion": 1,
    "navigation": {
        "title": "环境监测",
        "icon": "leaf",
        "order": 30,
        "route": "environment",
        "visibility": "instance-enabled"
    },
    "pages": [
        {
            "id": "home",
            "title": "环境监测",
            "sections": [
                {
                    "type": "status",
                    "source": "instance",
                    "title": "应用状态",
                    "description": "查看应用是否正常运行，以及最近一次状态同步。",
                    "emptyText": "暂时没有应用状态信息。"
                },
                {
                    "type": "metrics",
                    "source": "records",
                    "recordType": "environment",
                    "title": "当前环境",
                    "description": "显示最近一次收到的温度、光照和环境状态。",
                    "emptyText": "还没有收到环境数据，请确认设备已连接并开始上报。",
                    "fields": [
                        {
                            "key": "temperature.value",
                            "label": "温度",
                            "unit": "temperature.unit",
                            "precision": 1,
                            "hideWhenEmpty": True
                        },
                        {
                            "key": "illuminance.value",
                            "label": "光照",
                            "unit": "illuminance.unit",
                            "precision": 0,
                            "hideWhenEmpty": True
                        },
                        {
                            "key": "status",
                            "label": "环境状态",
                            "values": {
                                "within_thresholds": "正常",
                                "attention": "需要关注",
                                "stale": "数据已过期",
                                "unknown": "状态未知"
                            }
                        },
                        {
                            "key": "observed_at",
                            "label": "最近观测",
                            "format": "time",
                            "hideWhenEmpty": True
                        }
                    ]
                },
                {
                    "type": "actions",
                    "source": "manual-jobs",
                    "title": "手动操作",
                    "description": "需要时重新计算当前状态，不会重新读取传感器。",
                    "emptyText": "暂无可执行的操作。"
                },
                {
                    "type": "records",
                    "source": "records",
                    "recordType": "alert",
                    "presentation": "timeline",
                    "title": "变化记录",
                    "description": "查看温度和光照进入、离开设定范围的最近记录。",
                    "emptyText": "还没有温度或光照变化记录。",
                    "fields": [
                        {
                            "key": "title",
                            "label": "变化"
                        },
                        {
                            "key": "summary",
                            "label": "说明"
                        },
                        {
                            "key": "status",
                            "label": "状态",
                            "values": {
                                "entered": "进入设定范围",
                                "recovered": "已恢复正常"
                            }
                        },
                        {
                            "key": "condition",
                            "label": "类型",
                            "values": {
                                "temperature-high": "温度偏高",
                                "temperature-low": "温度偏低",
                                "light-below": "光照低于设定值",
                                "light-above": "光照高于设定值"
                            }
                        },
                        {
                            "key": "value",
                            "label": "读数",
                            "unit": "unit",
                            "precision": 1,
                            "hideWhenEmpty": True
                        }
                    ]
                },
                {
                    "type": "form",
                    "source": "config",
                    "fields": [
                        {
                            "key": "app_config.timezone",
                            "label": "时区",
                            "type": "string",
                            "description": "用于显示观测时间。填写 UTC 或 Asia/Shanghai 这样的时区名称。",
                            "placeholder": "Asia/Shanghai",
                            "default": "UTC"
                        },
                        {
                            "key": "app_config.temperature_min",
                            "label": "温度下限",
                            "type": "number",
                            "description": "低于这个温度时，环境状态会提示需要关注。数值使用传感器上报的单位。",
                            "default": 18
                        },
                        {
                            "key": "app_config.temperature_max",
                            "label": "温度上限",
                            "type": "number",
                            "description": "高于这个温度时，环境状态会提示需要关注。数值使用传感器上报的单位。",
                            "default": 28
                        },
                        {
                            "key": "app_config.light_threshold",
                            "label": "光照提醒值",
                            "type": "number",
                            "description": "需要光照提醒时填写。留空表示只显示读数，不判断是否超出范围。",
                            "placeholder": "例如 300"
                        },
                        {
                            "key": "app_config.light_alert_when",
                            "label": "光照提醒方向",
                            "type": "select",
                            "description": "选择低于还是高于设定值时提醒。这里只比较读数，不代表环境一定变暗或变亮。",
                            "enum": [
                                "below",
                                "above"
                            ],
                            "default": "below",
                            "values": {
                                "below": "低于设定值时提醒",
                                "above": "高于设定值时提醒"
                            }
                        },
                        {
                            "key": "app_config.hysteresis.temperature",
                            "label": "温度恢复缓冲",
                            "type": "number",
                            "description": "温度回到正常范围前需要越过的缓冲值，避免边界附近反复提醒。",
                            "minimum": 0,
                            "default": 1
                        },
                        {
                            "key": "app_config.hysteresis.light",
                            "label": "光照恢复缓冲",
                            "type": "number",
                            "description": "光照回到正常范围前需要越过的缓冲值，使用与光照读数相同的单位。",
                            "minimum": 0,
                            "default": 5
                        },
                        {
                            "key": "app_config.stale_after_s",
                            "label": "多久没有新数据就标记为过期（秒）",
                            "type": "integer",
                            "description": "超过这段时间没有收到新读数，页面会显示数据已过期。",
                            "minimum": 60,
                            "maximum": 86400,
                            "default": 120
                        },
                        {
                            "key": "app_config.temperature_unit",
                            "label": "温度单位",
                            "type": "string",
                            "description": "传感器没有提供单位时使用，例如 C 或 °C。应用不会换算温度。",
                            "placeholder": "例如 C"
                        },
                        {
                            "key": "app_config.light_unit",
                            "label": "光照单位",
                            "type": "string",
                            "description": "传感器没有提供单位时使用，例如 lux。应用不会换算光照。",
                            "placeholder": "例如 lux"
                        }
                    ],
                    "title": "提醒设置",
                    "description": "设置温度范围、光照提醒和读数过期时间。",
                    "emptyText": "暂无设置项。"
                }
            ],
            "description": "查看温度和光照是否在设定范围内，并了解最近一次变化。"
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


def validate(value):
    expected = {
        "apiVersion": "plugins.cloudpath.dev/v1alpha1",
        "kind": "Application", "id": PLUGIN_ID, "version": "0.1.5",
        "protocol": 1, "entrypoint": ENTRYPOINT,
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
        elif set(apps[0]) != {"id", "title", "ui"} or apps[0].get("id") != "environment-guard" or not isinstance(apps[0].get("title"), str) or not apps[0].get("title", "").strip():
            errors.append("application contribution identity/title is invalid")
        elif apps[0].get("ui") != UI:
            errors.append("application UI contribution is invalid")
    return errors


def imports(source):
    # Go parser-based assertions in manifest_test.go are the authoritative import
    # scan. This lightweight release gate also catches import blocks/singletons.
    for block in re.finditer(r'(?m)^import\s*(\([^)]*\)|[^\n]+)', source):
        yield from re.findall(r'"([^"\n]+)"', block.group(0))


def validate_tree(root, value):
    errors = validate(value)
    mirror = parse((root / "requirements.yaml").read_text(encoding="utf-8"))
    if mirror != {"requirements": REQUIREMENTS}:
        errors.append("requirements.yaml does not match the manifest")
    module = (root / "go.mod").read_text(encoding="utf-8")
    if not re.search(r'^module github.com/DeliciousBuding/cloud-path-app-environment-guard$', module, re.M):
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
    for key, bad in [("version", "0.0.0"), ("protocol", True), ("compatibility", {"core": ">=0.2.14 <0.3.0"}), ("permissions", {"network": ["*"]}), ("requirements", REQUIREMENTS[:1]), ("contributes", {"applications": [{"id": "bad"}]})]:
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
