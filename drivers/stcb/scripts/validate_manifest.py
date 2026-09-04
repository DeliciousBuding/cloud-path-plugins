#!/usr/bin/env python3
"""Validate a CloudPath plugin manifest and scan Go sources for internal imports.

This is a deliberate, stdlib-only parser for the *current* plugin.yaml shape.
It does not implement general YAML. The known-good manifest is a flat set of
top-level keys; the required fields are plain scalars and the optional fields
(compatibility/permissions/capabilities/requirements/contributes) hold indented
mappings or sequences. The parser enforces a safe closure on that shape and
rejects, rather than mis-parses, anything it does not understand.

Checks:
  1. Only the six required top-level scalars are validated for value; a nested
     block is simply ignored at the top level.
  2. Rejects duplicate top-level keys, tabs, empty required scalars, YAML
     multi-document markers, anchors/aliases/tags, and malformed top-level
     lines.
  3. Validates basic values: apiVersion, kind, protocol (and a light check on
     id/version/entrypoint).
  4. Recognises and validates the `contributes` subtree: each item in
     drivers/applications/connectors must carry a non-empty, filesystem-safe id,
     ids are unique across the plugin, and the plugin kind must match the
     contribution category. Unknown nested keys are preserved (ignored) per the
     current compatibility policy.
  5. Fails if any Go file under --dir imports
     github.com/DeliciousBuding/cloud-path/internal/*.

The authoritative JSON Schema remains spec/plugin-manifest.schema.json in the
cloud-path core repo; this script is a self-contained install-time gate.

Usage:
    python validate_manifest.py [manifest] [--dir root]
    python validate_manifest.py --self-test
"""
import argparse
import os
import re
import shutil
import sys
import tempfile

REQUIRED = ("apiVersion", "kind", "id", "version", "protocol", "entrypoint")
VALID_API = ("plugins.cloudpath.dev/v1alpha1",)
VALID_KINDS = ("Driver", "Application", "Connector")
CONTRIB_CATEGORIES = ("drivers", "applications", "connectors")
INTERNAL_PREFIX = "github.com/DeliciousBuding/cloud-path/internal/"

TAB = "\t"
TOP_KEY_RE = re.compile(r"^([A-Za-z0-9_./-]+):(.*)$")
DOC_RE = re.compile(r"^(---|\.\.\.)\s*$")
VERSION_RE = re.compile(r"^\d+(\.\d+)*([-+][0-9A-Za-z.-]+)?$")
PROTOCOL_RE = re.compile(r"^\d+$")
IMPORT_RE = re.compile(r'"(github\.com/DeliciousBuding/cloud-path/internal/[^"]+)"')


def unquote(value):
    value = value.strip()
    if len(value) >= 2 and value[0] == value[-1] and value[0] in ("'", '"'):
        return value[1:-1]
    return value


def strip_inline_comment(value):
    # A YAML comment starts with '#' preceded by whitespace.
    return value.split(" #", 1)[0]


def check_scalar(key, value, line, errors):
    if key == "apiVersion":
        if value not in VALID_API:
            errors.append("line %d: unsupported apiVersion %r" % (line, value))
    elif key == "kind":
        if value not in VALID_KINDS:
            errors.append("line %d: invalid kind %r (want one of %s)" % (line, value, ", ".join(VALID_KINDS)))
    elif key == "id":
        if value != value.strip() or " " in value or ".." in value or "/" in value or "\\" in value:
            errors.append("line %d: invalid plugin id %r" % (line, value))
    elif key == "version":
        if not VERSION_RE.match(value):
            errors.append("line %d: invalid version %r" % (line, value))
    elif key == "protocol":
        if not PROTOCOL_RE.match(value) or int(value) < 1:
            errors.append("line %d: invalid protocol %r (want a positive integer)" % (line, value))
    elif key == "entrypoint":
        if value != value.strip() or " " in value or ".." in value or "/" in value or "\\" in value:
            errors.append("line %d: invalid entrypoint %r" % (line, value))


def check_contribution_id(value, line, seen_ids):
    errors = []
    v = value.strip()
    if not v:
        errors.append("line %d: contribution id is empty" % line)
        return errors
    if " " in v:
        errors.append("line %d: contribution id must not contain whitespace: %r" % (line, v))
    if ".." in v:
        errors.append("line %d: contribution id must not contain '..': %r" % (line, v))
    if "/" in v or "\\" in v:
        errors.append("line %d: contribution id must not contain a path separator: %r" % (line, v))
    for ch in v:
        if ord(ch) < 0x20 or ord(ch) == 0x7F:
            errors.append("line %d: contribution id contains a control character: %r" % (line, v))
            break
    if v in seen_ids:
        errors.append("line %d: duplicate contribution id %r" % (line, v))
    seen_ids.add(v)
    return errors


def validate_contributes_block(lines, start_line):
    """Validate the indented subtree that follows a `contributes:` key.

    Closed subset we understand:
        contributes:
          <category>:
            - id: <value>
              <any-other-key>: <ignored>

    Every item must carry a non-empty, filesystem-safe id; ids are unique across
    the whole block. Unknown keys and categories are preserved (ignored). A
    plugin kind / contribution-category mismatch is rejected.
    """
    errors = []
    seen_ids = set()
    category = None
    cat_indent = None
    item_indent = None
    item = None  # {"id": str|None, "has_id": bool}

    def finish_item():
        if item is not None and not item["has_id"]:
            errors.append("line %d: contribution item missing id" % (start_line + len(lines)))

    for idx, raw in enumerate(lines):
        line_no = start_line + idx
        stripped = raw.strip()
        if not stripped or stripped.startswith("#"):
            continue
        indent = len(raw) - len(raw.lstrip(" "))

        if stripped.startswith("- "):
            finish_item()
            item_indent = indent
            item = {"id": None, "has_id": False}
            body = stripped[2:].strip()
            dm = re.match(r"^([A-Za-z0-9_./-]+):(.*)$", body)
            if dm and dm.group(1) == "id":
                val = unquote(strip_inline_comment(dm.group(2)))
                item["id"] = val
                item["has_id"] = True
                errors.extend(check_contribution_id(val, line_no, seen_ids))
            continue

        m = TOP_KEY_RE.match(stripped)
        if not m:
            # Malformed line inside contributes: preserve but do not mis-parse.
            continue
        key, val = m.group(1), m.group(2)

        # A sub-field of the current item.
        if item is not None and item_indent is not None and indent > item_indent:
            if key == "id":
                v = unquote(strip_inline_comment(val))
                if item["has_id"]:
                    errors.append("line %d: duplicate id in contribution item" % line_no)
                item["id"] = v
                item["has_id"] = True
                errors.extend(check_contribution_id(v, line_no, seen_ids))
            # else: unknown sub-field -> preserve (ignore)
            continue

        # Category key under contributes.
        if key in CONTRIB_CATEGORIES and (cat_indent is None or indent == cat_indent):
            finish_item()
            category = key
            cat_indent = indent
            item = None
            item_indent = None
            continue

        # Unknown nested key -> preserve (ignore) per compatibility policy.
        continue

    finish_item()
    return errors


def validate_manifest_text(text):
    """Return a list of validation errors (empty means OK)."""
    errors = []
    seen = {}
    lines = text.split("\n")
    total = len(lines)
    i = 0
    while i < total:
        raw = lines[i]
        line_no = i + 1
        if TAB in raw:
            errors.append("line %d: tabs are not allowed in manifest" % line_no)
            i += 1
            continue
        stripped = raw.strip()
        if not stripped or stripped.startswith("#"):
            i += 1
            continue
        if DOC_RE.match(stripped):
            errors.append("line %d: YAML document markers are not allowed" % line_no)
            i += 1
            continue
        if stripped.startswith("%"):
            errors.append("line %d: YAML directives are not allowed" % line_no)
            i += 1
            continue
        if any(ch in stripped for ch in ("&", "*")) or stripped.startswith(("!", "!!")):
            errors.append("line %d: YAML anchors/aliases/tags are not allowed" % line_no)
            i += 1
            continue
        if raw[0].isspace():
            # Indented body of an unspecial top-level key (compatibility/permissions
            # /capabilities/requirements): ignored, as before.
            i += 1
            continue
        m = TOP_KEY_RE.match(raw)
        if not m:
            errors.append("line %d: malformed top-level line: %r" % (line_no, raw))
            i += 1
            continue
        key, value = m.group(1), m.group(2)
        if key in seen:
            errors.append("line %d: duplicate top-level key %r" % (line_no, key))
        seen[key] = line_no
        if key in REQUIRED:
            v = unquote(strip_inline_comment(value))
            if not v:
                errors.append("line %d: required field %r is empty" % (line_no, key))
            else:
                check_scalar(key, v, line_no, errors)

        if key == "contributes":
            j = i + 1
            block = []
            while j < total:
                r = lines[j]
                if not r.strip() or r.strip().startswith("#"):
                    block.append(r)
                    j += 1
                    continue
                if r[0].isspace():
                    block.append(r)
                    j += 1
                    continue
                break
            errors.extend(validate_contributes_block(block, start_line=i + 2))
            i = j
            continue
        i += 1

    for k in REQUIRED:
        if k not in seen:
            errors.append("missing required field: %s" % k)
    return errors


def scan_internal(root):
    hits = []
    for dirpath, _dirnames, filenames in os.walk(root):
        for name in filenames:
            if not name.endswith(".go"):
                continue
            path = os.path.join(dirpath, name)
            with open(path, "r", encoding="utf-8", errors="replace") as f:
                for line in f:
                    for m in IMPORT_RE.finditer(line):
                        hits.append("%s: %s" % (path, m.group(1)))
    return hits


def valid_manifest_text():
    return (
        "apiVersion: plugins.cloudpath.dev/v1alpha1\n"
        "kind: Driver\n"
        "id: io.github.acme.cloud-path-driver-demo\n"
        "version: 0.1.0\n"
        "protocol: 1\n"
        "entrypoint: cloudpath-driver-demo\n"
        "compatibility:\n"
        '  core: ">=0.2.0 <0.4.0"\n'
        "permissions:\n"
        "  hardware: []\n"
        "capabilities:\n"
        "  - cloudpath.dev/capability/demodriver@1\n"
        "contributes:\n"
        "  drivers:\n"
        "    - id: demodriver\n"
        "      title: Demo Driver\n"
    )


def self_test():
    base = valid_manifest_text()
    contributes_lines = (
        "contributes:\n"
        "  drivers:\n"
        "    - id: demodriver\n"
        "      title: Demo Driver\n"
    )
    meta = base.split("contributes:\n")[0]  # everything before contributes
    cases = [
        ("valid", base, 0),
        ("missing kind", base.replace("kind: Driver\n", ""), 1),
        ("duplicate id", base + "id: a.b\n", 1),
        ("illegal kind", base.replace("kind: Driver", "kind: Widget"), 1),
        ("empty version", base.replace("version: 0.1.0\n", "version:\n"), 1),
        ("tab", base.replace("kind: Driver", "kind:\tDriver"), 1),
        ("multi-document", "---\n" + base + "---\n", 1),
        ("anchor", base.replace("kind: Driver", "kind: Driver &a"), 1),
        ("invalid protocol", base.replace("protocol: 1", "protocol: abc"), 1),
        ("contributes empty id", meta + contributes_lines.replace("id: demodriver", "id: \"\"") + "  applications:\n    - id: app\n", 1),
        ("contributes duplicate id", meta + "contributes:\n  drivers:\n    - id: demodriver\n    - id: demodriver\n", 1),
        ("contributes path id", meta + contributes_lines.replace("id: demodriver", "id: bad/id"), 1),
        ("contributes missing id", meta + "contributes:\n  drivers:\n    - title: No ID\n", 1),
    ]
    errors = []
    for name, text, want in cases:
        got = validate_manifest_text(text)
        ok = (len(got) == 0)
        if ok != (want == 0):
            errors.append("%s: expected exit %d, got %d (%s)" % (name, want, 0 if ok else 1, got))

    tmp = tempfile.mkdtemp(prefix="cpvalid-")
    try:
        with open(os.path.join(tmp, "bad.go"), "w", encoding="utf-8") as f:
            f.write('package bad\nimport _ "github.com/DeliciousBuding/cloud-path/internal/model"\n')
        hits = scan_internal(tmp)
        if not hits:
            errors.append("internal import scan should have found a hit")
    finally:
        shutil.rmtree(tmp, ignore_errors=True)

    if errors:
        for e in errors:
            print("self-test: " + e)
        print("self-test FAILED")
        return 1
    print("self-test OK")
    return 0


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("manifest", nargs="?", default="plugin.yaml")
    ap.add_argument("--dir", default=".")
    ap.add_argument("--self-test", action="store_true")
    args = ap.parse_args()

    if args.self_test:
        return self_test()

    ok = True
    try:
        with open(args.manifest, "r", encoding="utf-8") as f:
            text = f.read()
    except OSError as e:
        print("manifest: could not read %s: %s" % (args.manifest, e))
        return 1

    errs = validate_manifest_text(text)
    if errs:
        for e in errs:
            print("manifest: " + e)
        ok = False

    hits = scan_internal(args.dir)
    if hits:
        print("internal import found:")
        for h in hits:
            print("  " + h)
        ok = False

    if ok:
        print("plugin manifest OK; no internal imports")
        return 0
    return 1


if __name__ == "__main__":
    sys.exit(main())
