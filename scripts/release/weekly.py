"""Plan independent root, ADK, and GenAI Go module patch releases."""
from __future__ import annotations

import json
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
MODULES = {"root": "", "adk": "contrib/adk/", "genai": "contrib/genai/"}
VERSION = re.compile(r"^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$")
ROOT_PATHS = ("go.mod", "go.sum", ":(top,glob)*.go", "cmd", "internal", "contract")


def git(*args: str, check: bool = True) -> subprocess.CompletedProcess[str]:
    return subprocess.run(["git", *args], cwd=ROOT, text=True, capture_output=True, check=check)


def latest(prefix: str) -> tuple[str, str]:
    tags = git("tag", "--list", f"{prefix}v*").stdout.splitlines()
    candidates = []
    for tag in tags:
        match = VERSION.fullmatch(tag[len(prefix):])
        if match:
            candidates.append((tuple(map(int, match.groups())), tag))
    if not candidates:
        raise ValueError(f"No existing release tag for {prefix or 'root'}")
    parts, tag = max(candidates)
    git("merge-base", "--is-ancestor", tag, "HEAD")
    return tag, f"v{parts[0]}.{parts[1]}.{parts[2] + 1}"


def changed(tag: str, *paths: str) -> bool:
    result = git("diff", "--quiet", tag, "HEAD", "--", *paths, check=False)
    if result.returncode not in (0, 1):
        raise RuntimeError(result.stderr)
    return result.returncode == 1


def required_root(module: str) -> str:
    text = (ROOT / "contrib" / module / "go.mod").read_text()
    match = re.search(r"github\.com/neatlogs/neatlogs-go (v\d+\.\d+\.\d+)", text)
    if not match:
        raise ValueError(f"Could not find root dependency in contrib/{module}/go.mod")
    return match.group(1)


def plan() -> dict:
    tags = {name: latest(prefix) for name, prefix in MODULES.items()}
    release_root = changed(tags["root"][0], *ROOT_PATHS)
    root_version = tags["root"][1] if release_root else tags["root"][0]
    result = {"root": {"release": release_root, "version": root_version, "baseline": tags["root"][0]}}
    for name in ("adk", "genai"):
        baseline, next_version = tags[name]
        release = changed(baseline, f"contrib/{name}") or required_root(name) != root_version
        result[name] = {"release": release, "version": next_version if release else baseline[len(MODULES[name]):],
                        "baseline": baseline, "root_dependency": root_version}
    result["action"] = "publish" if any(result[name]["release"] for name in MODULES) else "skip"
    return result


if __name__ == "__main__":
    try:
        print(json.dumps(plan(), sort_keys=True))
    except (ValueError, RuntimeError, subprocess.CalledProcessError, OSError) as error:
        print(f"weekly Go release planning failed: {error}", file=sys.stderr)
        sys.exit(1)
