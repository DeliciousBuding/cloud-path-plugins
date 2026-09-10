#!/usr/bin/env python3
"""Build one plugin release from the monorepo catalog."""
from __future__ import annotations

import argparse
import hashlib
import os
import shutil
import subprocess
import sys
from pathlib import Path

from catalog import ROOT, find_by_tag

PLATFORMS = (
    ("linux", "amd64"),
    ("linux", "arm64"),
    ("darwin", "amd64"),
    ("darwin", "arm64"),
    ("windows", "amd64"),
    ("windows", "arm64"),
)


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def run(command: list[str], cwd: Path | None = None, env: dict[str, str] | None = None) -> None:
    result = subprocess.run(command, cwd=cwd, env=env, text=True)
    if result.returncode:
        raise RuntimeError(f"command failed ({result.returncode}): {' '.join(command)}")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--tag", required=True)
    parser.add_argument("--out", required=True)
    args = parser.parse_args()
    try:
        plugin, version = find_by_tag(args.tag)
        module_dir = ROOT / plugin.path
        validator = module_dir / "scripts" / "validate_manifest.py"
        if validator.is_file():
            run([sys.executable, str(validator), "plugin.yaml", "--dir", "."], cwd=module_dir)
        out_dir = Path(args.out).resolve()
        repo_root = ROOT.resolve()
        if out_dir != repo_root and repo_root not in out_dir.parents:
            raise ValueError(f"output must stay inside {repo_root}")
        if out_dir.exists():
            shutil.rmtree(out_dir)
        out_dir.mkdir(parents=True)
        for goos, goarch in PLATFORMS:
            suffix = ".exe" if goos == "windows" else ""
            name = f"{plugin.asset}_{version}_{goos}_{goarch}{suffix}"
            env = os.environ.copy()
            env.update({"CGO_ENABLED": "0", "GOOS": goos, "GOARCH": goarch})
            run(
                [
                    "go",
                    "build",
                    "-trimpath",
                    "-ldflags=-s -w",
                    "-o",
                    str(out_dir / name),
                    f"./cmd/{plugin.entrypoint}",
                ],
                cwd=module_dir,
                env=env,
            )
            print(f"{name}\t{sha256(out_dir / name)}")
        shutil.copy2(module_dir / "plugin.yaml", out_dir / "plugin.yaml")
        shutil.copy2(ROOT / "LICENSE", out_dir / "LICENSE")
        shutil.copy2(ROOT / "NOTICE", out_dir / "NOTICE")
        shutil.copy2(ROOT / "THIRD_PARTY_NOTICES.md", out_dir / "THIRD_PARTY_NOTICES.md")
        checksum_lines = []
        for artifact in sorted(out_dir.iterdir(), key=lambda p: p.name):
            if artifact.name != "checksums.txt" and artifact.is_file():
                checksum_lines.append(f"{sha256(artifact)}  {artifact.name}")
        (out_dir / "checksums.txt").write_text("\n".join(checksum_lines) + "\n", encoding="utf-8")
        print(f"release: {plugin.slug} {version} -> {out_dir}")
        return 0
    except (OSError, RuntimeError, ValueError) as exc:
        print(f"release: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
