#!/usr/bin/env python3
"""Manual real-board E2E for the CloudPath Sensor Alert Application.

This script is intentionally not suitable for unattended execution. It uses only
the CloudPath REST API, creates a unique isolated plugin instance, and asks the
operator to produce the physical sensor change. It never opens a serial port.

Default invocation is a dry-run. Real execution requires --execute, a TTY, and
an explicit confirmation phrase. This acceptance path is deliberately LED-only:
it never binds alert-sound and never sends tone/buzzer commands. Credentials are
read from environment variables or a key=value file and are never printed.
"""
from __future__ import annotations

import argparse
import datetime as dt
import getpass
import hashlib
import http.client
import json
import os
from pathlib import Path
import sys
import time
import urllib.error
import urllib.parse
import urllib.request


ROOT = Path(__file__).resolve().parents[1]
PLUGIN_FILE = ROOT / "plugin.yaml"


def manifest_defaults():
    try:
        manifest = json.loads(PLUGIN_FILE.read_text(encoding="utf-8"))
        return str(manifest["id"]), str(manifest["version"])
    except (OSError, KeyError, TypeError, ValueError):
        return "io.github.deliciousbuding.cloud-path-app-sensor-alert", "0.2.1"


PLUGIN_ID, PLUGIN_VERSION = manifest_defaults()

SENSOR_CAPABILITY = {
    "temperature": "cloudpath.dev/capability/temperature@1",
    "illuminance": "cloudpath.dev/capability/illuminance@1",
    "contact": "cloudpath.dev/capability/hall@1",
    "vibration": "cloudpath.dev/capability/vibration@1",
}
SENSOR_REQUIREMENT = {
    "temperature": "temperature",
    "illuminance": "illuminance",
    "contact": "contact",
    "vibration": "vibration",
}
LED_CAPABILITY = "cloudpath.dev/capability/led@1"


class ApiError(RuntimeError):
    def __init__(self, status, body):
        self.status = int(status)
        self.body = body if isinstance(body, dict) else {"error": str(body)}
        code = self.body.get("code") or self.body.get("error") or "http_error"
        message = self.body.get("message") or self.body.get("error") or ""
        message = str(message).replace("\r", " ").replace("\n", " ")[:300]
        super().__init__("HTTP %d %s%s" % (self.status, code, (": " + message) if message else ""))


class Api:
    def __init__(self, base_url, timeout=20):
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout
        self.cookie = ""

    def request(self, method, path, body=None, timeout=None):
        data = None
        if body is not None:
            data = json.dumps(body, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
        request = urllib.request.Request(self.base_url + path, data=data, method=method)
        request.add_header("User-Agent", "cloudpath-sensor-alert-e2e/1.0")
        if data is not None:
            request.add_header("Content-Type", "application/json")
        if self.cookie:
            request.add_header("Cookie", self.cookie)
        try:
            with urllib.request.urlopen(request, timeout=timeout or self.timeout) as response:
                raw = response.read()
                set_cookie = response.headers.get("Set-Cookie")
                if set_cookie:
                    self.cookie = set_cookie.split(";", 1)[0]
                if not raw:
                    return {}
                try:
                    return json.loads(raw.decode("utf-8"))
                except ValueError as exc:
                    raise RuntimeError("CloudPath returned non-JSON data") from exc
        except urllib.error.HTTPError as exc:
            raw = exc.read()
            try:
                body = json.loads(raw.decode("utf-8")) if raw else {}
            except (UnicodeDecodeError, ValueError):
                body = {"error": raw.decode("utf-8", "replace")[:300]}
            raise ApiError(exc.code, body) from None
        except (urllib.error.URLError, TimeoutError, http.client.RemoteDisconnected) as exc:
            raise RuntimeError("CloudPath request failed: %s" % (exc,)) from None

    def get(self, path, timeout=None):
        last_error = None
        for attempt in range(3):
            try:
                return self.request("GET", path, timeout=timeout)
            except ApiError as exc:
                last_error = exc
                if exc.status not in (429, 502, 503, 504) or attempt == 2:
                    raise
            except RuntimeError as exc:
                last_error = exc
                if attempt == 2:
                    raise
            time.sleep(1.0 + attempt)
        raise RuntimeError("GET failed: %s" % (last_error,))


def quote(value):
    return urllib.parse.quote(str(value), safe="")


def load_credentials_file(path):
    values = {}
    for raw_line in Path(path).read_text(encoding="utf-8").splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#"):
            continue
        if line.startswith("export "):
            line = line[7:].strip()
        if "=" not in line:
            continue
        key, value = line.split("=", 1)
        value = value.strip()
        if len(value) >= 2 and value[0] == value[-1] and value[0] in ("'", '"'):
            value = value[1:-1]
        values[key.strip()] = value
    return values


def load_credentials(args):
    values = {}
    if args.credentials_file:
        values.update(load_credentials_file(args.credentials_file))
    username = os.environ.get("CLOUDPATH_ADMIN_USERNAME") or values.get("CLOUDPATH_ADMIN_USERNAME")
    password = os.environ.get("CLOUDPATH_ADMIN_PASSWORD") or values.get("CLOUDPATH_ADMIN_PASSWORD")
    if username and not password and sys.stdin.isatty():
        password = getpass.getpass("CloudPath admin password: ")
    if not username or not password:
        raise RuntimeError(
            "set CLOUDPATH_ADMIN_USERNAME and CLOUDPATH_ADMIN_PASSWORD, "
            "or pass --credentials-file"
        )
    return username, password


def parse_device(value):
    if not value or "/" not in value:
        raise RuntimeError("--device/CLOUDPATH_E2E_DEVICE must be <edge_id>/<device_id>")
    edge, device = value.split("/", 1)
    if not edge or not device:
        raise RuntimeError("--device/CLOUDPATH_E2E_DEVICE must be <edge_id>/<device_id>")
    return edge, device


def device_path(edge, device):
    return "/api/devices/%s/%s" % (quote(edge), quote(device))


def device_key(edge, device):
    return "%s/%s" % (edge, device)


def parse_json(value, fallback):
    if not value:
        return fallback
    try:
        return json.loads(value)
    except (TypeError, ValueError):
        return fallback


def choose_entity(descriptor, capability, override, label):
    entities = descriptor.get("entities", []) if isinstance(descriptor, dict) else []
    if override:
        for entity in entities:
            if entity.get("entity_id") == override:
                if capability not in (entity.get("capabilities") or []):
                    raise RuntimeError("%s override %s does not expose %s" % (label, override, capability))
                return override
        raise RuntimeError("%s override %s was not found in the descriptor" % (label, override))
    matches = [
        str(entity.get("entity_id")) for entity in entities
        if capability in (entity.get("capabilities") or [])
    ]
    if len(matches) != 1:
        raise RuntimeError("expected exactly one %s entity for %s, got %s" % (label, capability, matches))
    return matches[0]


def state_value(device, entity, prop):
    state = device.get("state") if isinstance(device, dict) else {}
    if not isinstance(state, dict):
        return None
    value = state.get("%s.%s" % (entity, prop))
    if isinstance(value, dict):
        value = value.get("value")
    return value


def numeric_state(device, entity, prop, label):
    value = state_value(device, entity, prop)
    if isinstance(value, bool):
        raise RuntimeError("device state %s.%s is not numeric: %r" % (entity, prop, value))
    try:
        return float(value)
    except (TypeError, ValueError):
        raise RuntimeError("device state %s.%s is missing or not numeric: %r" % (entity, prop, value))


def build_config(args, device, sensor_entity):
    cfg = {
        "temperature_min": 18,
        "temperature_max": 28,
        "light_min": None,
        "light_max": None,
        "contact_enabled": False,
        "vibration_enabled": False,
        "cooldown_s": 0,
        "silent": True,
        "alert_led_mask": 85,
        "alert_tone": None,
    }
    if args.sensor == "temperature":
        current = numeric_state(device, sensor_entity, "value", "temperature")
        if args.direction == "high":
            cfg["temperature_min"] = current - 100.0
            cfg["temperature_max"] = current + 1.0
        else:
            cfg["temperature_min"] = current - 1.0
            cfg["temperature_max"] = current + 100.0
    elif args.sensor == "illuminance":
        current = numeric_state(device, sensor_entity, "value", "illuminance")
        delta = 10.0
        if args.direction == "high":
            if current + delta > 255:
                raise RuntimeError("illuminance is too high for a high-side trigger; use --direction low")
            cfg["light_max"] = current + delta
        else:
            if current - delta < 0:
                raise RuntimeError("illuminance is too low for a low-side trigger; use --direction high")
            cfg["light_min"] = current - delta
    elif args.sensor == "contact":
        cfg["contact_enabled"] = True
    elif args.sensor == "vibration":
        cfg["vibration_enabled"] = True
    return cfg


def trigger_instruction(args, cfg):
    if args.sensor == "vibration":
        return "轻敲或晃动 STC-B 板，使振动传感器出现 quake/state=1；不要按 Reset。"
    if args.sensor == "contact":
        return "把磁铁靠近霍尔传感器，使状态变为 close/state=1；保持磁铁就位。"
    if args.sensor == "illuminance":
        if args.direction == "high":
            return "用手机手电或强光照射光敏传感器，使读数超过 light_max=%s。" % cfg["light_max"]
        return "遮挡光敏传感器，使读数低于 light_min=%s。" % cfg["light_min"]
    if args.direction == "high":
        return "用手捂热温度传感器，使读数超过 temperature_max=%s。" % cfg["temperature_max"]
    return "让温度传感器降温，使读数低于 temperature_min=%s。" % cfg["temperature_min"]


def recover_instruction(args):
    if args.sensor == "vibration":
        return "让板子保持静止几秒，等待振动状态回到 0。"
    if args.sensor == "contact":
        return "移开磁铁，使霍尔状态回到 away/state=0。"
    if args.sensor == "illuminance":
        return "恢复环境光照，使读数回到配置阈值范围内。"
    return "恢复环境温度，使读数回到配置阈值范围内。"


def operator_confirm(message):
    print("\n" + message)
    input("准备好后按 Enter 开始等待；脚本不会自动执行这一步。 ")
    print("现在执行上述物理动作，脚本正在等待设备事件。")


def wait_for(fn, predicate, timeout, description, interval=1.0):
    deadline = time.monotonic() + timeout
    last = None
    last_error = None
    while time.monotonic() < deadline:
        try:
            last = fn()
            if predicate(last):
                return last
        except Exception as exc:
            last_error = exc
        time.sleep(interval)
    detail = last_error if last_error is not None else last
    raise RuntimeError("timed out waiting for %s; last=%s" % (description, str(detail)[:600]))


def wait_instance_running(api, instance_id, timeout):
    path = "/api/plugin-instances/%s" % quote(instance_id)
    def ready():
        view = api.get(path)
        observed = view.get("observed") if isinstance(view, dict) else None
        return view, observed or {}
    def predicate(value):
        view, observed = value
        return view.get("drift") is False and observed.get("state") == "running"
    value = wait_for(ready, predicate, timeout, "instance %s running" % instance_id)
    return value[0]


def wait_bindings(api, instance_id, expected, timeout):
    path = "/api/plugin-instances/%s/bindings" % quote(instance_id)
    def read():
        return api.get(path)
    def predicate(view):
        if not view.get("running"):
            return False
        actual = set()
        for binding in view.get("bindings", []):
            actual.add((str(binding.get("requirement_id")), str(binding.get("entity_id"))))
        return all(item in actual for item in expected)
    return wait_for(read, predicate, timeout, "instance bindings")


def run_job(api, instance_id, job_id, key):
    path = "/api/plugin-instances/%s/jobs/%s/run" % (quote(instance_id), quote(job_id))
    reply = api.request("POST", path, {"args_json": "{}", "idempotency_key": key})
    return parse_json(reply.get("result_json"), {})


def make_key(instance_id, label):
    digest = hashlib.sha256(("%s|%s|%d" % (instance_id, label, time.time_ns())).encode("utf-8")).hexdigest()[:16]
    return "e2e-%s-%s" % (label, digest)


def list_commands(api, edge, device):
    path = "/api/commands?device=%s&limit=200" % quote(device_key(edge, device))
    return api.get(path).get("commands", [])


def max_command_id(commands):
    return max([int(row.get("id", 0)) for row in commands] or [0])


def wait_acked_command(api, edge, device, baseline_id, action, expected_args, timeout):
    deadline = time.monotonic() + timeout
    last_matches = []
    while time.monotonic() < deadline:
        matches = []
        for row in list_commands(api, edge, device):
            if int(row.get("id", 0)) <= baseline_id or row.get("cmd") != action:
                continue
            args = parse_json(row.get("args"), None)
            if expected_args is not None and args != expected_args:
                continue
            matches.append(row)
            if row.get("status") == "ok" and "device ack" in str(row.get("result") or "").lower():
                return row
            if row.get("status") in ("failed", "timeout"):
                raise RuntimeError("command %s failed before ACK: %s" % (action, row))
        last_matches = matches[-3:]
        time.sleep(1.0)
    raise RuntimeError("timed out waiting for %s device ACK; candidates=%s" % (action, last_matches))


def alert_record(api, instance_id):
    path = "/api/plugin-instances/%s/records?record_type=alert&limit=50" % quote(instance_id)
    rows = api.get(path).get("records", [])
    rows = [row for row in rows if row.get("record_type") == "alert" and row.get("record_id") == "current"]
    if not rows:
        return None
    rows.sort(key=lambda row: int(row.get("updated_at", 0)), reverse=True)
    return parse_json(rows[0].get("data_json"), None)


def wait_alert_state(api, instance_id, state, sensor, timeout):
    def read():
        return alert_record(api, instance_id)
    def predicate(record):
        return isinstance(record, dict) and record.get("state") == state and (not sensor or record.get("sensor") == sensor)
    return wait_for(read, predicate, timeout, "alert record %s" % state, interval=0.25)


def wait_settled_status(api, instance_id, timeout):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        last = run_job(api, instance_id, "status", make_key(instance_id, "status"))
        pending = last.get("pending_commands") or []
        last_command = last.get("last_command") or {}
        if not pending and last_command.get("state") == "succeeded":
            return last
        time.sleep(1.0)
    raise RuntimeError("commands did not settle via RequestCompleted; status=%s" % last)


def delete_instance(api, instance_id, purge):
    path = "/api/plugin-instances/%s" % quote(instance_id)
    return api.request("DELETE", path, {"purge": bool(purge)})


def execute(args):
    edge, device = parse_device(args.device or os.environ.get("CLOUDPATH_E2E_DEVICE"))
    base_url = args.base_url or os.environ.get("CLOUDPATH_BASE_URL")
    if not base_url:
        raise RuntimeError("set CLOUDPATH_BASE_URL or pass --base-url")
    username, password = load_credentials(args)
    instance_id = args.instance_id or "sensor-alert-e2e-%d" % int(time.time())
    evidence = {
        "kind": "sensor-alert-real-board-e2e",
        "started_at": dt.datetime.now(dt.timezone.utc).isoformat(),
        "device": device_key(edge, device),
        "plugin": {"id": args.plugin_id, "version": args.plugin_version},
        "sensor": args.sensor,
        "direction": args.direction,
        "instance_id": instance_id,
        "cleanup": {},
    }
    api = Api(base_url, timeout=args.request_timeout)
    created = False
    result_code = 1
    cleanup_errors = []
    try:
        api.request("POST", "/api/auth/login", {"username": username, "password": password})
        health = api.get("/healthz")
        if not health.get("ok"):
            raise RuntimeError("CloudPath health check failed: %s" % health)
        device_view = api.get(device_path(edge, device))
        if not device_view.get("online"):
            raise RuntimeError("target device is offline: %s" % device_key(edge, device))
        descriptor_response = api.get(device_path(edge, device) + "/descriptor")
        descriptor = descriptor_response.get("descriptor", {})
        sensor_entity = choose_entity(descriptor, SENSOR_CAPABILITY[args.sensor], args.sensor_entity, "sensor")
        led_entity = choose_entity(descriptor, LED_CAPABILITY, args.led_entity, "LED")
        cfg = build_config(args, device_view, sensor_entity)
        evidence["config"] = cfg
        evidence["entities"] = {"sensor": sensor_entity, "led": led_entity}
        bindings = [
            {"requirement_id": SENSOR_REQUIREMENT[args.sensor], "entity_id": sensor_entity},
            {"requirement_id": "alert-light", "entity_id": led_entity},
        ]
        payload = {
            "edge_id": args.app_edge,
            "instance_id": instance_id,
            "plugin_id": args.plugin_id,
            "version": args.plugin_version,
            "enabled": True,
            "config": {
                "app_config": json.dumps(cfg, ensure_ascii=False, separators=(",", ":")),
                "app_bindings": json.dumps(bindings, ensure_ascii=False, separators=(",", ":")),
            },
        }
        api.request("POST", "/api/plugin-instances", payload)
        created = True
        evidence["create"] = "ok"
        wait_instance_running(api, instance_id, args.timeout)
        expected_bindings = set((item["requirement_id"], item["entity_id"]) for item in bindings)
        wait_bindings(api, instance_id, expected_bindings, args.timeout)
        evidence["arm"] = run_job(api, instance_id, "arm", make_key(instance_id, "arm"))
        if evidence["arm"].get("armed") is not True:
            raise RuntimeError("arm did not report armed=true: %s" % evidence["arm"])
        baseline = max_command_id(list_commands(api, edge, device))
        evidence["baseline_command_id"] = baseline

        operator_confirm(trigger_instruction(args, cfg))
        triggered = wait_alert_state(api, instance_id, "triggered", args.sensor, args.action_timeout)
        evidence["triggered_record"] = triggered
        led = wait_acked_command(api, edge, device, baseline, "led", {"mask": cfg["alert_led_mask"]}, args.timeout)
        time.sleep(1.0)
        audible = [
            row for row in list_commands(api, edge, device)
            if int(row.get("id", 0)) > baseline and row.get("cmd") in ("tone", "buzzer")
        ]
        if audible:
            raise RuntimeError("LED-only E2E detected audible commands: %s" % audible)
        evidence["trigger_commands"] = {"led": led, "audible": []}
        evidence["request_completed_status"] = wait_settled_status(api, instance_id, args.timeout)

        baseline_after_trigger = max_command_id(list_commands(api, edge, device))
        if args.recovery_mode == "disarm":
            evidence["disarm"] = run_job(api, instance_id, "disarm", make_key(instance_id, "disarm"))
            if evidence["disarm"].get("armed") is not False:
                raise RuntimeError("disarm did not report armed=false: %s" % evidence["disarm"])
            recovered = wait_alert_state(api, instance_id, "disarmed", "", args.timeout)
            evidence["recovery"] = {"mode": "disarm", "record": recovered}
        else:
            operator_confirm(recover_instruction(args))
            recovered = wait_alert_state(api, instance_id, "recovered", args.sensor, args.action_timeout)
            evidence["recovery"] = {"mode": "recover", "record": recovered}
        led_off = wait_acked_command(api, edge, device, baseline_after_trigger, "led", {"mask": 0}, args.timeout)
        evidence["recovery_command"] = led_off
        evidence["ok"] = True
        result_code = 0
    except Exception as exc:
        evidence["ok"] = False
        evidence["error"] = str(exc)[:1200]
        print("E2E failed: %s" % evidence["error"], file=sys.stderr)
        result_code = 1
    finally:
        cleanup = evidence["cleanup"]
        if created:
            try:
                run_job(api, instance_id, "disarm", make_key(instance_id, "cleanup-disarm"))
                cleanup["disarm"] = "ok"
            except Exception as exc:
                cleanup["disarm_error"] = str(exc)[:300]
                cleanup_errors.append("disarm: %s" % cleanup["disarm_error"])
            if not args.keep_instance:
                try:
                    delete_instance(api, instance_id, purge=True)
                    cleanup["delete"] = "ok"
                except Exception as exc:
                    cleanup["delete_error"] = str(exc)[:300]
                    cleanup_errors.append("delete: %s" % cleanup["delete_error"])
            else:
                cleanup["delete"] = "kept by --keep-instance"
        try:
            api.request("POST", "/api/auth/logout", timeout=8)
            cleanup["logout"] = "ok"
        except Exception as exc:
            cleanup["logout_error"] = str(exc)[:300]
        if cleanup_errors:
            evidence["ok"] = False
            evidence["cleanup_errors"] = cleanup_errors
            result_code = 1
            print("E2E cleanup failed: %s" % "; ".join(cleanup_errors), file=sys.stderr)
        evidence["finished_at"] = dt.datetime.now(dt.timezone.utc).isoformat()
        evidence_path = Path(args.evidence) if args.evidence else ROOT / ".local" / "validation" / ("sensor-alert-e2e-%s.json" % int(time.time()))
        evidence_path.parent.mkdir(parents=True, exist_ok=True)
        evidence_path.write_text(json.dumps(evidence, ensure_ascii=False, indent=2), encoding="utf-8")
        print("evidence: %s" % evidence_path)
    return result_code


def parse_args():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--execute", action="store_true", help="perform the real-board E2E; otherwise print a dry-run plan")
    parser.add_argument("--base-url", help="CloudPath base URL (or CLOUDPATH_BASE_URL)")
    parser.add_argument("--device", help="real device <edge_id>/<device_id> (or CLOUDPATH_E2E_DEVICE)")
    parser.add_argument("--credentials-file", help="key=value file containing CLOUDPATH_ADMIN_USERNAME/PASSWORD")
    parser.add_argument("--instance-id", help="unique isolated instance id; default is timestamped")
    parser.add_argument("--app-edge", default="server", help="edge_id that hosts the Application plugin (default: server)")
    parser.add_argument("--plugin-id", default=PLUGIN_ID)
    parser.add_argument("--plugin-version", default=PLUGIN_VERSION)
    parser.add_argument("--sensor", choices=sorted(SENSOR_CAPABILITY), default="vibration")
    parser.add_argument("--direction", choices=("high", "low"), default="high", help="threshold direction for temperature/illuminance")
    parser.add_argument("--sensor-entity", help="override discovered sensor entity id")
    parser.add_argument("--led-entity", help="override discovered LED entity id")
    parser.add_argument("--recovery-mode", choices=("disarm", "recover"), default="disarm")
    parser.add_argument("--timeout", type=float, default=90.0, help="API/effect timeout in seconds")
    parser.add_argument("--action-timeout", type=float, default=120.0, help="physical action timeout in seconds")
    parser.add_argument("--request-timeout", type=float, default=20.0)
    parser.add_argument("--evidence", help="evidence JSON path; default is gitignored .local/validation/")
    parser.add_argument("--keep-instance", action="store_true", help="do not delete the isolated instance after the run")
    return parser.parse_args()


def main():
    args = parse_args()
    if not args.execute:
        print("DRY-RUN: no API calls, no real-board action.")
        print("Planned isolated instance: %s" % (args.instance_id or "sensor-alert-e2e-<timestamp>"))
        print("Planned sensor: %s (%s)" % (args.sensor, args.direction))
        print("Audible output: disabled (silent=true, alert_tone=null, no alert-sound binding)")
        print("Required env: CLOUDPATH_BASE_URL, CLOUDPATH_E2E_DEVICE, CLOUDPATH_ADMIN_USERNAME, CLOUDPATH_ADMIN_PASSWORD")
        print("Run with --execute in an interactive terminal to perform the physical E2E.")
        return 0
    if not sys.stdin.isatty():
        print("refusing --execute without an interactive TTY (unattended execution is disabled)", file=sys.stderr)
        return 2
    answer = input("This will create an isolated instance and operate the real board. Type RUN-SENSOR-ALERT-E2E to continue: ")
    if answer.strip() != "RUN-SENSOR-ALERT-E2E":
        print("confirmation declined", file=sys.stderr)
        return 2
    try:
        return execute(args)
    except Exception as exc:
        print("E2E setup failed: %s" % str(exc)[:1200], file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
