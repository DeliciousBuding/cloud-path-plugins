#!/usr/bin/env python3
"""Manual real-board E2E for the Hall Pillbox application.

This script deliberately has no unattended mode.  It only talks to the
CloudPath REST API, never opens a serial port, and requires an interactive
terminal because the hall ``close`` and ``away``/``open`` events must be
produced by moving the physical magnet.

The script never prints credential values.  Run without ``--execute`` first to
see the exact mutation plan.  This flow emits buzzer commands, so execution is
fail-closed: a real run additionally requires the explicit ``--allow-audible``
flag.  When the production ``box-prod`` instance is active, it also requires
``--takeover-box-prod`` so the script can temporarily release the exclusive
buzzer binding and restore it in ``finally``.
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable, Iterable, Optional


PLUGIN_ID = "io.github.deliciousbuding.cloud-path-app-hall-pillbox"
DEFAULT_BASE_URL = "https://cloudpath.vectorcontrol.tech"
DEFAULT_DEVICE = "lab-edge/stcb-real-1"
DEFAULT_BOX_PROD = "box-prod"
DEFAULT_APP_EDGE = "server"
DEFAULT_PLUGIN_VERSION = "0.1.1"

HALL_CAPABILITY = "cloudpath.dev/capability/hall@1"
HALL_CLOSE = HALL_CAPABILITY + "/close"
HALL_AWAY = HALL_CAPABILITY + "/away"
HALL_OPEN = HALL_CAPABILITY + "/open"

TERMINAL_COMMAND_STATES = {"succeeded", "failed", "timedout", "cancelled"}
TERMINAL_REMINDER_STATES = TERMINAL_COMMAND_STATES


class E2EError(RuntimeError):
    """A deterministic E2E failure with a user-readable message."""


class ConflictError(E2EError):
    """A production instance conflicts with the isolated test instance."""


@dataclass(frozen=True)
class Credentials:
    username: str
    password: str


@dataclass
class BoxProdState:
    existed: bool
    original_enabled: bool
    needs_takeover: bool = False
    taken_over: bool = False


class CloudPathApi:
    def __init__(self, base_url: str, timeout: float = 20.0) -> None:
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout
        self.cookie: Optional[str] = None

    def request(
        self,
        method: str,
        path: str,
        body: Any = None,
        timeout: Optional[float] = None,
    ) -> tuple[int, Any]:
        data = None
        if body is not None:
            data = json.dumps(body, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
        request = urllib.request.Request(self.base_url + path, data=data, method=method)
        request.add_header("Accept", "application/json")
        request.add_header("User-Agent", "cloudpath-hall-pillbox-e2e/1.0")
        if data is not None:
            request.add_header("Content-Type", "application/json")
        if self.cookie:
            request.add_header("Cookie", self.cookie)

        try:
            with urllib.request.urlopen(request, timeout=timeout or self.timeout) as response:
                raw = response.read().decode("utf-8", "replace")
                set_cookie = response.headers.get("Set-Cookie")
                if set_cookie:
                    self.cookie = set_cookie.split(";", 1)[0]
                return response.status, json.loads(raw) if raw else {}
        except urllib.error.HTTPError as exc:
            raw = exc.read().decode("utf-8", "replace")
            try:
                payload = json.loads(raw) if raw else {}
            except json.JSONDecodeError:
                payload = {}
            return exc.code, payload
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            raise E2EError(
                f"HTTP request failed: {method} {path}: {getattr(exc, 'reason', exc)}"
            ) from exc


def parse_env_file(path: Path) -> dict[str, str]:
    values: dict[str, str] = {}
    for raw_line in path.read_text(encoding="utf-8").splitlines():
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


def load_credentials(credentials_file: str) -> Credentials:
    username = os.environ.get("CLOUDPATH_ADMIN_USERNAME", "").strip()
    password = os.environ.get("CLOUDPATH_ADMIN_PASSWORD", "")

    if not username or not password:
        path = Path(credentials_file).expanduser()
        if not path.is_file():
            raise E2EError(
                "credentials are missing: set CLOUDPATH_ADMIN_USERNAME and "
                "CLOUDPATH_ADMIN_PASSWORD, or provide --credentials-file"
            )
        values = parse_env_file(path)
        username = username or values.get("CLOUDPATH_ADMIN_USERNAME", "").strip()
        password = password or values.get("CLOUDPATH_ADMIN_PASSWORD", "")

    if not username or not password:
        raise E2EError(
            "credentials are incomplete: CLOUDPATH_ADMIN_USERNAME and "
            "CLOUDPATH_ADMIN_PASSWORD are both required"
        )
    return Credentials(username=username, password=password)


def error_summary(payload: Any) -> str:
    if not isinstance(payload, dict):
        return "no structured error body"
    parts: list[str] = []
    for key in ("code", "error", "request_id"):
        value = payload.get(key)
        if isinstance(value, str) and value and value not in parts:
            parts.append(f"{key}={value}")
    return ", ".join(parts) if parts else "no structured error body"


def call(
    api: CloudPathApi,
    method: str,
    path: str,
    body: Any = None,
    allowed: Iterable[int] = (200,),
    timeout: Optional[float] = None,
) -> Any:
    code, payload = api.request(method, path, body=body, timeout=timeout)
    allowed_set = set(allowed)
    if code not in allowed_set:
        raise E2EError(
            f"{method} {path} returned HTTP {code}: {error_summary(payload)}"
        )
    return payload


def instance_path(instance_id: str) -> str:
    return "/api/plugin-instances/" + urllib.parse.quote(instance_id, safe="")


def get_instance(api: CloudPathApi, instance_id: str) -> Optional[dict[str, Any]]:
    code, payload = api.request("GET", instance_path(instance_id))
    if code == 404:
        return None
    if code != 200:
        raise E2EError(
            f"GET {instance_path(instance_id)} returned HTTP {code}: {error_summary(payload)}"
        )
    if not isinstance(payload, dict):
        raise E2EError("instance response was not a JSON object")
    return payload


def get_bindings(api: CloudPathApi, instance_id: str) -> dict[str, Any]:
    payload = call(api, "GET", instance_path(instance_id) + "/bindings")
    if not isinstance(payload, dict):
        raise E2EError("bindings response was not a JSON object")
    return payload


def get_records(api: CloudPathApi, instance_id: str, record_type: str = "window") -> dict[str, Any]:
    query = urllib.parse.urlencode({"record_type": record_type, "limit": 100})
    payload = call(api, "GET", instance_path(instance_id) + "/records?" + query)
    if not isinstance(payload, dict):
        raise E2EError("records response was not a JSON object")
    return payload


def get_events(
    api: CloudPathApi,
    device: str,
    since: int = 0,
    limit: int = 1000,
) -> list[dict[str, Any]]:
    query = urllib.parse.urlencode({"device": device, "since": since, "limit": limit})
    payload = call(api, "GET", "/api/events?" + query)
    rows = payload.get("events", []) if isinstance(payload, dict) else []
    if not isinstance(rows, list):
        raise E2EError("events response did not contain a list")
    return [row for row in rows if isinstance(row, dict)]


def run_job(
    api: CloudPathApi,
    instance_id: str,
    job_id: str,
    args: dict[str, Any],
    idempotency_key: str,
) -> dict[str, Any]:
    path = instance_path(instance_id) + "/jobs/" + urllib.parse.quote(job_id, safe="") + "/run"
    payload = call(
        api,
        "POST",
        path,
        body={
            "args_json": json.dumps(args, ensure_ascii=False, separators=(",", ":")),
            "idempotency_key": idempotency_key,
        },
    )
    raw = payload.get("result_json", "") if isinstance(payload, dict) else ""
    try:
        result = json.loads(raw or "{}")
    except json.JSONDecodeError as exc:
        raise E2EError(f"job {job_id} returned invalid result_json") from exc
    if not isinstance(result, dict):
        raise E2EError(f"job {job_id} result was not a JSON object")
    return result


def desired(instance: Optional[dict[str, Any]]) -> dict[str, Any]:
    if not instance or not isinstance(instance.get("desired"), dict):
        return {}
    return instance["desired"]


def observed(instance: Optional[dict[str, Any]]) -> dict[str, Any]:
    if not instance or not isinstance(instance.get("observed"), dict):
        return {}
    return instance["observed"]


def is_running(instance: Optional[dict[str, Any]]) -> bool:
    return bool(desired(instance).get("enabled")) and observed(instance).get("state") == "running"


def wait_for(
    label: str,
    fetch: Callable[[], Any],
    predicate: Callable[[Any], bool],
    timeout: float,
    interval: float = 1.0,
) -> Any:
    deadline = time.monotonic() + timeout
    last: Any = None
    while True:
        last = fetch()
        if predicate(last):
            return last
        if time.monotonic() >= deadline:
            break
        time.sleep(interval)
    raise E2EError(f"{label} timed out after {timeout:.0f}s")


def wait_instance_running(api: CloudPathApi, instance_id: str, timeout: float) -> dict[str, Any]:
    return wait_for(
        f"instance {instance_id} running",
        lambda: get_instance(api, instance_id),
        lambda value: value is not None and is_running(value) and not value.get("drift"),
        timeout,
    )


def wait_bindings(
    api: CloudPathApi,
    instance_id: str,
    expected: dict[str, str],
    timeout: float,
) -> dict[str, Any]:
    def predicate(payload: dict[str, Any]) -> bool:
        if not payload.get("running"):
            return False
        got = {
            (binding.get("requirement_id"), binding.get("entity_id"))
            for binding in payload.get("bindings", [])
            if isinstance(binding, dict)
        }
        return all((requirement, entity) in got for requirement, entity in expected.items())

    return wait_for(
        f"instance {instance_id} bindings",
        lambda: get_bindings(api, instance_id),
        predicate,
        timeout,
    )


def record_data(payload: dict[str, Any], record_id: str) -> Optional[dict[str, Any]]:
    for row in payload.get("records", []):
        if not isinstance(row, dict) or row.get("record_id") != record_id:
            continue
        raw = row.get("data_json", "")
        if isinstance(raw, dict):
            return raw
        try:
            data = json.loads(raw or "{}")
        except json.JSONDecodeError as exc:
            raise E2EError(f"record {record_id} has invalid data_json") from exc
        if not isinstance(data, dict):
            raise E2EError(f"record {record_id} data_json is not an object")
        return data
    return None


def wait_record(
    api: CloudPathApi,
    instance_id: str,
    record_id: str,
    predicate: Callable[[dict[str, Any]], bool],
    timeout: float,
    label: str,
) -> dict[str, Any]:
    return wait_for(
        label,
        lambda: record_data(get_records(api, instance_id), record_id),
        lambda value: value is not None and predicate(value),
        timeout,
    )


def assert_record_stays(
    api: CloudPathApi,
    instance_id: str,
    record_id: str,
    predicate: Callable[[dict[str, Any]], bool],
    duration: float,
    label: str,
) -> dict[str, Any]:
    deadline = time.monotonic() + duration
    last: Optional[dict[str, Any]] = None
    while time.monotonic() < deadline:
        last = record_data(get_records(api, instance_id), record_id)
        if last is None or not predicate(last):
            raise E2EError(
                f"{label} changed unexpectedly: "
                + json.dumps(last, ensure_ascii=False, sort_keys=True)
            )
        time.sleep(0.5)
    if last is None:
        raise E2EError(f"{label} did not produce a record")
    return last


def event_payload(row: dict[str, Any]) -> dict[str, Any]:
    raw = row.get("payload", "")
    if isinstance(raw, dict):
        return raw
    try:
        payload = json.loads(raw or "{}")
    except json.JSONDecodeError:
        return {}
    return payload if isinstance(payload, dict) else {}


def event_type(row: dict[str, Any]) -> str:
    payload = event_payload(row)
    value = payload.get("type")
    if isinstance(value, str) and value:
        return value
    value = row.get("type")
    return value if isinstance(value, str) else ""


def event_entity(row: dict[str, Any]) -> str:
    value = event_payload(row).get("entity_id")
    return value if isinstance(value, str) else ""


def event_id(row: dict[str, Any]) -> int:
    try:
        return int(row.get("id", 0))
    except (TypeError, ValueError):
        return 0


def event_ts(row: dict[str, Any]) -> int:
    try:
        return int(row.get("ts", 0))
    except (TypeError, ValueError):
        return 0


def event_cursor(api: CloudPathApi, device: str) -> tuple[int, int]:
    rows = get_events(api, device, since=0, limit=1000)
    if not rows:
        return 0, int(time.time())
    newest = max(rows, key=event_id)
    return event_id(newest), event_ts(newest)


def wait_hall_event(
    api: CloudPathApi,
    device: str,
    after_id: int,
    after_ts: int,
    event_types: set[str],
    timeout: float,
    label: str,
) -> dict[str, Any]:
    since = max(0, after_ts - 5)

    def fetch() -> list[dict[str, Any]]:
        return [
            row
            for row in get_events(api, device, since=since, limit=1000)
            if event_id(row) > after_id
            and event_type(row) in event_types
            and event_entity(row) == "hall"
        ]

    rows = wait_for(label, fetch, lambda value: bool(value), timeout)
    return min(rows, key=event_id)


def prepare_box_prod(api: CloudPathApi, args: argparse.Namespace) -> BoxProdState:
    current = get_instance(api, args.box_prod_instance)
    if current is None:
        return BoxProdState(existed=False, original_enabled=False)

    desired_state = desired(current)
    observed_state = observed(current)
    enabled = bool(desired_state.get("enabled"))
    running = observed_state.get("state") == "running"
    if not enabled and not running:
        print(f"box-prod {args.box_prod_instance} 未启用，跳过冲突接管")
        return BoxProdState(existed=True, original_enabled=enabled)

    if not args.takeover_box_prod:
        raise ConflictError(
            f"检测到 {args.box_prod_instance} enabled={enabled} "
            f"observed={observed_state.get('state', 'unknown')}，它可能占用 buzzer。"
            "真板隔离测试需要显式加 --takeover-box-prod；脚本会临时禁用并在 finally 恢复。"
        )

    print(
        f"冲突提示：临时禁用 {args.box_prod_instance}（原 enabled={enabled}），"
        "隔离实例结束后会在 finally 恢复。"
    )
    return BoxProdState(existed=True, original_enabled=enabled, needs_takeover=True)


def takeover_box_prod(
    api: CloudPathApi,
    args: argparse.Namespace,
    box_state: BoxProdState,
) -> None:
    # Mark the takeover before the network write: if the response is lost, the
    # finally path still restores the original enabled state.
    box_state.taken_over = True
    call(
        api,
        "PATCH",
        instance_path(args.box_prod_instance),
        body={"enabled": False},
    )


def wait_box_prod_stopped(api: CloudPathApi, args: argparse.Namespace) -> None:
    wait_for(
        f"{args.box_prod_instance} stopped",
        lambda: get_instance(api, args.box_prod_instance),
        lambda value: value is not None
        and not desired(value).get("enabled")
        and observed(value).get("state") != "running"
        and not value.get("drift"),
        args.timeout,
    )


def cleanup(
    api: CloudPathApi,
    args: argparse.Namespace,
    box_state: Optional[BoxProdState],
    instance_created: bool,
) -> list[str]:
    errors: list[str] = []

    if instance_created:
        try:
            code, payload = api.request("DELETE", instance_path(args.instance_id))
            if code not in (200, 204, 404):
                errors.append(
                    f"delete {args.instance_id}: HTTP {code}: {error_summary(payload)}"
                )
            else:
                wait_for(
                    f"delete {args.instance_id} observed",
                    lambda: get_instance(api, args.instance_id),
                    lambda value: value is None,
                    args.timeout,
                )
        except E2EError as exc:
            errors.append(str(exc))

    if box_state is not None and box_state.taken_over and box_state.original_enabled:
        try:
            call(
                api,
                "PATCH",
                instance_path(args.box_prod_instance),
                body={"enabled": True},
            )
            wait_instance_running(api, args.box_prod_instance, args.timeout)
        except E2EError as exc:
            errors.append(f"restore {args.box_prod_instance}: {exc}")

    return errors


def logout(api: CloudPathApi) -> None:
    if not api.cookie:
        return
    try:
        api.request("POST", "/api/auth/logout", timeout=8)
    except E2EError:
        pass
    finally:
        api.cookie = None


def arm_physical_event(message: str) -> None:
    input(message + " 准备好后按 Enter，脚本随即开始计时：")


def selected_record_fields(record: dict[str, Any]) -> dict[str, Any]:
    keys = (
        "id",
        "state",
        "confirmation_source",
        "opened_at",
        "closed_at",
        "reminder_state",
        "reminder_result",
        "reminder_error_code",
        "reminder_stop_state",
        "reminder_stop_request_id",
        "reminder_stop_result",
        "reminder_stop_error_code",
    )
    return {key: record.get(key) for key in keys}


def selected_event_fields(row: dict[str, Any]) -> dict[str, Any]:
    return {"id": event_id(row), "ts": event_ts(row), "type": event_type(row)}


def execute(args: argparse.Namespace) -> dict[str, Any]:
    if not getattr(args, "allow_audible", False):
        raise E2EError(
            "refusing to run Hall Pillbox E2E: it emits buzzer commands; "
            "pass --allow-audible only after explicit approval"
        )
    api = CloudPathApi(args.base_url, timeout=args.http_timeout)
    credentials = load_credentials(args.credentials_file)
    box_state: Optional[BoxProdState] = None
    instance_created = False
    primary_error: Optional[BaseException] = None
    result: dict[str, Any] = {}
    try:
        login = call(
            api,
            "POST",
            "/api/auth/login",
            body={"username": credentials.username, "password": credentials.password},
            allowed=(200, 204),
        )
        if not isinstance(login, dict):
            raise E2EError("login response was not a JSON object")

        call(api, "GET", "/healthz")

        devices = call(api, "GET", "/api/devices").get("devices", [])
        device = next(
            (
                item
                for item in devices
                if isinstance(item, dict)
                and f"{item.get('edge_id', '')}/{item.get('id', '')}" == args.device
            ),
            None,
        )
        if device is None:
            raise E2EError(f"device {args.device} was not found")
        if not device.get("online"):
            raise E2EError(f"device {args.device} is offline")

        call(api, "GET", "/api/plugins/" + urllib.parse.quote(PLUGIN_ID, safe=""))

        if get_instance(api, args.instance_id) is not None:
            raise E2EError(
                f"isolated instance {args.instance_id} already exists; choose a new --instance-id"
            )

        box_state = prepare_box_prod(api, args)
        if box_state.needs_takeover:
            takeover_box_prod(api, args, box_state)
            wait_box_prod_stopped(api, args)

        try:
            call(
                api,
                "POST",
                "/api/plugin-instances",
                body={
                    "edge_id": args.app_edge,
                    "instance_id": args.instance_id,
                    "plugin_id": PLUGIN_ID,
                    "version": args.plugin_version,
                    "enabled": True,
                    "config": {
                        "app_config": json.dumps(args.app_config, ensure_ascii=False, separators=(",", ":")),
                        "app_bindings": json.dumps(args.app_bindings, ensure_ascii=False, separators=(",", ":")),
                    },
                },
                allowed=(200, 201),
            )
        except BaseException:
            # A timeout can arrive after the server committed the create.  Treat
            # the isolated instance as ours so finally can still remove it.
            try:
                current = get_instance(api, args.instance_id)
                current_desired = desired(current)
                if (
                    current is not None
                    and current_desired.get("plugin_id") == PLUGIN_ID
                    and current_desired.get("instance_id") == args.instance_id
                ):
                    instance_created = True
            except E2EError:
                pass
            raise
        instance_created = True
        wait_instance_running(api, args.instance_id, args.timeout)
        wait_bindings(api, args.instance_id, args.expected_bindings, args.timeout)

        print(
            "开始真板霍尔 E2E。请让磁铁离开霍尔（盖开/away）并保持，"
            "随后按提示先做 close 忽略，再做 away/open 确认。"
        )
        arm_physical_event("步骤 1/4：保持磁铁离开霍尔，等待窗口开启。")

        start_result = run_job(
            api,
            args.instance_id,
            "start-window",
            {"window_id": args.window_id, "minutes": args.window_minutes},
            args.instance_id + "-start",
        )
        if start_result.get("state") != "opened":
            raise E2EError(f"start-window returned unexpected result: {start_result}")

        wait_record(
            api,
            args.instance_id,
            args.window_id,
            lambda value: value.get("state") == "opened",
            args.timeout,
            "window opened record",
        )
        wait_record(
            api,
            args.instance_id,
            args.window_id,
            lambda value: value.get("reminder_state") in TERMINAL_REMINDER_STATES,
            args.timeout,
            "start buzzer terminal receipt",
        )
        start_record = wait_record(
            api,
            args.instance_id,
            args.window_id,
            lambda value: value.get("reminder_state") == "succeeded",
            args.timeout,
            "start buzzer succeeded",
        )

        close_after_id, close_after_ts = event_cursor(api, args.device)
        arm_physical_event(
            "步骤 2/4：把磁铁靠近霍尔（盖关/close），产生 close 边沿。"
        )
        close_event = wait_hall_event(
            api,
            args.device,
            close_after_id,
            close_after_ts,
            {HALL_CLOSE},
            args.event_timeout,
            "hall close event",
        )
        close_record = assert_record_stays(
            api,
            args.instance_id,
            args.window_id,
            lambda value: value.get("state") == "opened"
            and value.get("confirmation_source") in (None, "")
            and not value.get("reminder_stop_request_id"),
            min(5.0, args.timeout),
            "close ignored record",
        )

        arm_physical_event(
            "步骤 3/4：把磁铁再次移开霍尔（盖开/away），产生 away 或兼容 open 边沿。"
        )
        away_event = wait_hall_event(
            api,
            args.device,
            event_id(close_event),
            event_ts(close_event),
            {HALL_AWAY, HALL_OPEN},
            args.event_timeout,
            "hall away/open event",
        )
        completed_record = wait_record(
            api,
            args.instance_id,
            args.window_id,
            lambda value: value.get("state") == "completed"
            and value.get("confirmation_source") == "hall"
            and bool(value.get("closed_at"))
            and bool(value.get("reminder_stop_request_id")),
            args.timeout,
            "hall confirmation record",
        )
        stop_record = wait_record(
            api,
            args.instance_id,
            args.window_id,
            lambda value: value.get("reminder_stop_state") in TERMINAL_COMMAND_STATES,
            args.timeout,
            "stop buzzer terminal receipt",
        )

        print("步骤 4/4：断言通过。")
        result = {
            "status": "PASS",
            "instance_id": args.instance_id,
            "device": args.device,
            "events": {
                "close": selected_event_fields(close_event),
                "away_or_open": selected_event_fields(away_event),
            },
            "records": {
                "after_start": selected_record_fields(start_record),
                "after_close_ignored": selected_record_fields(close_record),
                "after_away_open": selected_record_fields(completed_record),
                "after_stop_receipt": selected_record_fields(stop_record),
            },
        }
    except BaseException as exc:
        primary_error = exc
        raise
    finally:
        cleanup_errors = cleanup(api, args, box_state, instance_created)
        logout(api)
        for message in cleanup_errors:
            print(f"cleanup error: {message}", file=sys.stderr)
        if cleanup_errors and primary_error is None:
            raise E2EError("cleanup failed: " + "; ".join(cleanup_errors))

    return result


def build_app_config() -> dict[str, Any]:
    # Put the unused daily schedule at least 12 hours away so a manual run does
    # not accidentally create a scheduled window.  The explicit start-window job
    # is the only window used by this script.
    shanghai_hour = (dt.datetime.now(dt.timezone.utc).hour + 8) % 24
    start_hour = (shanghai_hour + 12) % 24
    start = f"{start_hour:02d}:00"
    end = f"{start_hour:02d}:01"
    return {
        "timezone": "Asia/Shanghai",
        "compartment": "e2e-hall",
        "schedule": [{"id": "e2e-unused", "start": start, "end": end}],
        "reminder": {"freq": 1, "duration": 1},
    }


def build_app_bindings() -> list[dict[str, str]]:
    return [
        {"requirement_id": "opening", "entity_id": "hall"},
        {"requirement_id": "confirm", "entity_id": "key1"},
        {"requirement_id": "reminder-output", "entity_id": "buzzer"},
    ]


def expected_bindings() -> dict[str, str]:
    return {"opening": "hall", "confirm": "key1", "reminder-output": "buzzer"}


def generated_instance_id() -> str:
    stamp = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    return f"hall-e2e-{stamp}-{uuid.uuid4().hex[:6]}"


def parse_args(argv: Optional[list[str]] = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description=(
            "Manual real-board E2E for Hall Pillbox. Dry-run by default; "
            "requires --execute and an interactive terminal for physical events."
        )
    )
    parser.add_argument("--execute", action="store_true", help="perform REST writes and require physical events")
    parser.add_argument(
        "--takeover-box-prod",
        action="store_true",
        help="temporarily disable the production box-prod instance and restore it in finally",
    )
    parser.add_argument(
        "--allow-audible",
        action="store_true",
        help="explicitly allow this E2E to send buzzer commands; default is fail-closed",
    )
    parser.add_argument("--base-url", default=os.environ.get("CLOUDPATH_BASE_URL", DEFAULT_BASE_URL))
    parser.add_argument(
        "--credentials-file",
        default=os.environ.get("CLOUDPATH_CREDENTIALS_FILE", ""),
    )
    parser.add_argument("--device", default=os.environ.get("CLOUDPATH_E2E_DEVICE", DEFAULT_DEVICE))
    parser.add_argument("--app-edge", default=os.environ.get("CLOUDPATH_E2E_APP_EDGE", DEFAULT_APP_EDGE))
    parser.add_argument("--box-prod-instance", default=os.environ.get("CLOUDPATH_E2E_BOX_PROD", DEFAULT_BOX_PROD))
    parser.add_argument("--plugin-version", default=os.environ.get("CLOUDPATH_E2E_PLUGIN_VERSION", DEFAULT_PLUGIN_VERSION))
    parser.add_argument("--instance-id", default="")
    parser.add_argument("--window-id", default="")
    parser.add_argument("--window-minutes", type=int, default=10)
    parser.add_argument("--timeout", type=float, default=90.0, help="REST convergence timeout in seconds")
    parser.add_argument("--event-timeout", type=float, default=120.0, help="physical event timeout in seconds")
    parser.add_argument("--http-timeout", type=float, default=20.0)
    args = parser.parse_args(argv)
    if not args.instance_id:
        args.instance_id = generated_instance_id()
    if not args.window_id:
        args.window_id = "e2e-" + args.instance_id
    if not 1 <= args.window_minutes <= 120:
        parser.error("--window-minutes must be between 1 and 120")
    if not args.device or "/" not in args.device:
        parser.error("--device must be <edge_id>/<device_id>")
    args.app_config = build_app_config()
    args.app_bindings = build_app_bindings()
    args.expected_bindings = expected_bindings()
    return args


def print_plan(args: argparse.Namespace) -> None:
    print("DRY RUN：未读取凭据、未连接 CloudPath、未产生任何写操作。")
    print(f"base_url={args.base_url}")
    print(f"device={args.device}")
    print(f"isolated_instance={args.instance_id}")
    print(f"plugin={PLUGIN_ID}@{args.plugin_version}")
    print("写操作计划：创建隔离实例；禁用并恢复 box-prod（仅在 --takeover-box-prod 时）；finally 删除隔离实例。")
    print("真板手动步骤：磁铁离开 -> start-window -> close（应忽略）-> away/open（应确认）。")
    print("注意：该流程会发 buzzer 命令；默认拒绝执行，只有显式 --allow-audible 才会运行。")
    print("执行真板 E2E：python scripts/e2e_hall_pillbox.py --execute --takeover-box-prod --allow-audible")


def main(argv: Optional[list[str]] = None) -> int:
    args = parse_args(argv)
    if not args.execute:
        print_plan(args)
        return 0
    if not args.allow_audible:
        print(
            "E2E REFUSED: Hall Pillbox emits buzzer commands; pass --allow-audible only after explicit approval.",
            file=sys.stderr,
        )
        return 2
    if not sys.stdin.isatty() or not sys.stdout.isatty():
        print(
            "E2E FAILED: 真板事件需要交互式终端；拒绝在无人值守环境执行。",
            file=sys.stderr,
        )
        return 2
    try:
        result = execute(args)
    except ConflictError as exc:
        print(f"CONFLICT: {exc}", file=sys.stderr)
        return 2
    except E2EError as exc:
        print(f"E2E FAILED: {exc}", file=sys.stderr)
        return 1
    except EOFError:
        print("E2E FAILED: 物理事件步骤需要交互输入。", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print("E2E INTERRUPTED：finally 清理已执行。", file=sys.stderr)
        return 130
    print(json.dumps(result, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
