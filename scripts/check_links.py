#!/usr/bin/env python3
"""Validate relative links in tracked Markdown files.

Only inline links of the form ``[text](target)`` are checked. A target is
treated as relative when it does not start with a URL scheme, ``//``, ``/``,
``#``, ``<``, or a Windows drive letter. Relative targets are resolved against
the location of the Markdown file and are reported as broken when the resolved
path (after stripping any ``#fragment`` or ``?query``) is not a file or
directory.

Reference-style links (``[text][ref]``), autolinks (``<https://...>``),
absolute URLs and fragment-only anchors are intentionally left alone. The check
is stdlib-only and uses ``git ls-files`` so local state and worktrees are out of
scope.

Usage:
    python scripts/check_links.py            # check all tracked .md files
    python scripts/check_links.py --self-test
"""
from __future__ import annotations

import argparse
import os
import re
import shutil
import subprocess
import sys
import tempfile
import urllib.parse
from dataclasses import dataclass


@dataclass(frozen=True)
class Finding:
    path: str
    line: int
    link: str
    resolved: str


LINK_RE = re.compile(r"!?\[[^\]]*\]\(([^)]*)\)")
SCHEME_RE = re.compile(r"^[A-Za-z][A-Za-z0-9+.-]*:")
DRIVE_RE = re.compile(r"^[A-Za-z]:")


def is_relative(target: str) -> bool:
    t = target.strip()
    if not t:
        return False
    if t.startswith(("//", "/", "#", "<")):
        return False
    if SCHEME_RE.match(t):
        return False
    if DRIVE_RE.match(t):
        return False
    return True


def resolve_target(md_path: str, target: str, md_dir: str) -> str:
    t = target.strip()
    if t.startswith("<") and t.endswith(">"):
        t = t[1:-1]
    # Drop a trailing markdown title ("...") after the path.
    parts = t.split()
    if len(parts) > 1:
        t = parts[0]
    # Strip fragment and query for the existence check.
    t = t.split("#", 1)[0].split("?", 1)[0]
    t = t.replace("\\", "/")
    if not t:
        return ""
    t = urllib.parse.unquote(t)
    return os.path.normpath(os.path.join(md_dir, t))


def check_file(md_path: str) -> list[Finding]:
    try:
        with open(md_path, "r", encoding="utf-8", errors="replace") as f:
            lines = f.readlines()
    except OSError:
        return []
    md_dir = os.path.dirname(os.path.abspath(md_path))
    findings: list[Finding] = []
    for line_no, line in enumerate(lines, 1):
        for m in LINK_RE.finditer(line):
            target = m.group(1)
            if not is_relative(target):
                continue
            resolved = resolve_target(md_path, target, md_dir)
            if not resolved or not os.path.exists(resolved):
                findings.append(Finding(md_path, line_no, target.strip(), resolved))
    return findings


def tracked_markdown() -> list[str]:
    proc = subprocess.run(["git", "ls-files", "-z"], capture_output=True, check=False)
    if proc.returncode != 0:
        raise RuntimeError(proc.stderr.decode("utf-8", "replace").strip())
    return [
        part.decode("utf-8", "surrogateescape")
        for part in proc.stdout.split(b"\0")
        if part and part.endswith(b".md")
    ]


def self_test() -> int:
    tmp = tempfile.mkdtemp(prefix="cplinks-")
    errors: list[str] = []
    try:
        good = os.path.join(tmp, "good.md")
        with open(good, "w", encoding="utf-8") as f:
            f.write("[ok](good.md)\n[anchor](#top)\n[ext](https://example.com/x)\n")
        good_findings = check_file(good)
        if good_findings:
            errors.append("good file produced findings")

        bad = os.path.join(tmp, "bad.md")
        with open(bad, "w", encoding="utf-8") as f:
            f.write("[broken](missing-file.md)\n")
        bad_findings = check_file(bad)
        if not bad_findings:
            errors.append("broken link was not detected")
        elif bad_findings[0].link != "missing-file.md":
            errors.append("broken link finding reported the wrong target")
    finally:
        shutil.rmtree(tmp, ignore_errors=True)

    if errors:
        for e in errors:
            print("self-test: " + e)
        print("self-test FAILED")
        return 1
    print("markdown link check self-test PASS")
    return 0


def collect_md(paths: list[str]) -> list[str]:
    md_files: list[str] = []
    for p in paths:
        if os.path.isfile(p):
            md_files.append(p)
        elif os.path.isdir(p):
            for root, _dirs, files in os.walk(p):
                for f in files:
                    if f.endswith(".md"):
                        md_files.append(os.path.join(root, f))
        else:
            print("path not found: " + p, file=sys.stderr)
            raise SystemExit(1)
    return md_files


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--self-test", action="store_true")
    parser.add_argument("paths", nargs="*", help="optional dirs/files to check")
    args = parser.parse_args()
    if args.self_test:
        return self_test()

    if args.paths:
        md_files = collect_md(args.paths)
    else:
        md_files = tracked_markdown()

    findings: list[Finding] = []
    for f in md_files:
        findings.extend(check_file(f))

    if findings:
        print("markdown link check FAILED:")
        for x in findings:
            print(f"  {x.path}:{x.line} -> broken relative link '{x.link}' (resolved {x.resolved})")
        return 1
    print(f"markdown link check PASS ({len(md_files)} files)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
