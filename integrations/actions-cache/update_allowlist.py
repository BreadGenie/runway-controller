#!/usr/bin/env python3
"""Resolve moving major tags; append immutable pins. Python stdlib, python3 -I safe."""
import argparse
import hashlib
import json
import os
import posixpath
from pathlib import Path
import re
import urllib.request

ROOT = Path(__file__).resolve().parents[2]
ALLOWLIST = ROOT / "cmd/cache-node/allowlist.json"


def fetch(url):
    headers = {"Accept": "application/vnd.github+json", "User-Agent": "runway-cache-allowlist"}
    if url.startswith("https://api.github.com/") and os.environ.get("GH_TOKEN"):
        headers["Authorization"] = "Bearer " + os.environ["GH_TOKEN"]
    with urllib.request.urlopen(urllib.request.Request(url, headers=headers), timeout=60) as response:
        return response.read()


def tags(repo, get):
    result = []
    page = 1
    while True:
        batch = json.loads(get(f"https://api.github.com/repos/{repo}/tags?per_page=100&page={page}"))
        result.extend(batch)
        if len(batch) < 100:
            return result
        page += 1


def candidates(entries, get=fetch):
    result = []
    for repo in ("actions/cache", "actions/setup-node"):
        pins = [e for e in entries if e["repo"] == repo]
        oldest = min(int(e["version"].split(".")[0][1:]) for e in pins)
        all_tags = tags(repo, get)
        for tag in all_tags:
            if not re.fullmatch(r"v[1-9][0-9]*", tag["name"]) or int(tag["name"][1:]) < oldest:
                continue
            commit = tag["commit"]["sha"]
            if not re.fullmatch(r"[0-9a-f]{40}", commit):
                raise ValueError("invalid commit")
            if any(e["commit"] == commit for e in pins):
                continue
            releases = sorted(t["name"] for t in all_tags if t["commit"]["sha"] == commit and re.fullmatch(r"v[1-9][0-9]*\.[0-9]+\.[0-9]+", t["name"]))
            if len(releases) != 1:
                raise ValueError(f"{repo} {tag['name']}: expected one release at {commit}: {releases}")
            item = (repo, releases[0], commit)
            if item not in result:
                result.append(item)
    return sorted(result)


def manifest_paths(data, prefix=""):
    # Deliberately support only the upstream Node action manifest subset.
    text = data.decode()
    if re.search(r"(?m)^\s+pre\s*:", text):
        raise ValueError("pre entrypoint requires harness support")
    paths = []
    for line in text.splitlines():
        match = re.fullmatch(r"\s+(main|post):\s*['\"]?([A-Za-z0-9_./-]+)['\"]?\s*", line)
        if match:
            path = posixpath.normpath(prefix + match[2])
            if not re.fullmatch(r"dist/[a-z-]+/index\.js", path):
                raise ValueError("unsupported entrypoint: " + path)
            paths.append(path)
    if not paths or len(paths) != len(set(paths)):
        raise ValueError("missing or duplicate entrypoints")
    return paths


def resolve(item, get=fetch):
    repo, version, commit = item
    base = f"https://raw.githubusercontent.com/{repo}/{commit}/"
    paths = []
    manifest = get(base + "action.yml")
    paths.extend(manifest_paths(manifest))
    if repo == "actions/cache":
        for name in ("restore", "save"):
            paths.extend(manifest_paths(get(base + name + "/action.yml"), name + "/"))
    else:
        paths.append("action.yml")
    expected = ({"dist/restore/index.js", "dist/save/index.js", "dist/restore-only/index.js", "dist/save-only/index.js"}
                if repo == "actions/cache" else {"dist/setup/index.js", "dist/cache-save/index.js", "action.yml"})
    if set(paths) != expected or len(paths) != len(expected):
        raise ValueError("entrypoints require harness support: " + repr(paths))
    return [dict(repo=repo, version=version, commit=commit, path=p,
                 sha256=hashlib.sha256(manifest if p == "action.yml" else get(base + p)).hexdigest(),
                 harness_only=p == "action.yml") for p in paths]


def provision(entries, directory, get=fetch):
    for e in entries:
        prefix = "actions-cache" if e["repo"] == "actions/cache" else "setup-node"
        name = "action.yml" if e["harness_only"] else e["path"].split("/")[1] + "-index.js"
        path = directory / (prefix + "-" + e["commit"][:8]) / name
        if path.exists() and hashlib.sha256(path.read_bytes()).hexdigest() == e["sha256"]:
            continue
        data = get(f"https://raw.githubusercontent.com/{e['repo']}/{e['commit']}/{e['path']}")
        if hashlib.sha256(data).hexdigest() != e["sha256"]:
            raise ValueError("hash mismatch: " + str(path))
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("--fetch-dir", type=Path)
    args = parser.parse_args()
    entries = json.loads(ALLOWLIST.read_text())
    found = candidates(entries)
    print("no new candidates" if not found else json.dumps(found))
    additions = [e for item in found for e in resolve(item)]
    if not args.dry_run:
        if additions:
            ALLOWLIST.write_text(json.dumps(entries + additions, indent=2) + "\n")
        if args.fetch_dir:
            provision(entries + additions, args.fetch_dir)
    if os.environ.get("GITHUB_OUTPUT"):
        identity = "-".join(repo.split("/")[1] + "-" + version for repo, version, _ in found)
        with open(os.environ["GITHUB_OUTPUT"], "a") as output:
            output.write("branch=" + ("bot/cache-allowlist-" + identity if found else "") + "\n")
    if os.environ.get("GITHUB_STEP_SUMMARY"):
        with open(os.environ["GITHUB_STEP_SUMMARY"], "a") as summary:
            summary.write("## Cache allowlist candidates\n" + json.dumps(additions, indent=2) + "\n")


if __name__ == "__main__":
    main()
