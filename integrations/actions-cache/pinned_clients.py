"""Offline pinned action clients shared by the relay integration test."""
import hashlib
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess

ROOT = Path(__file__).resolve().parents[2]
VENDOR = Path(os.environ.get('RUNWAY_CACHE_VENDOR', ROOT / '.scratch/runway-regional-cache/vendor'))
NODE_DEFAULT = shutil.which('node') or os.path.expanduser('~/.local/opt/node/bin/node')
YARN_BIN = os.environ.get('YARN_BIN') or shutil.which('yarn') or ''


def pinned_version(kind, release, commit, hashes):
    """Describe full-commit upstream files without performing network I/O."""
    bundles = {}
    for name, digest in hashes.items():
        extension = "cjs" if kind == "setup-uv" and int(release.split(".")[0][1:]) >= 7 else "js"
        path = "action.yml" if name == "action.yml" else "dist/" + name + "/index." + extension
        repo = "astral-sh/setup-uv" if kind == "setup-uv" else "actions/" + kind
        bundles[name] = {
            "url": "https://raw.githubusercontent.com/" + repo + "/" + commit + "/" + path,
            "sha256": digest,
            "filename": "action.yml" if name == "action.yml" else name + "-index." + extension,
        }
    prefix = "actions-cache" if kind == "cache" else kind
    return {"release": release, "commit": commit,
            "directory": prefix + "-" + commit[:8], "bundles": bundles}


def load_versions():
    import json
    grouped = {}
    for entry in json.loads((ROOT / "cmd/cache-node/allowlist.json").read_text()):
        kind = entry["repo"].split("/")[1]
        key = (kind, entry["version"], entry["commit"])
        name = "action.yml" if entry["harness_only"] else entry["path"].split("/")[1]
        grouped.setdefault(key, {})[name] = entry["sha256"]
    result = {}
    for (kind, release, commit), hashes in grouped.items():
        result.setdefault(kind, []).append(pinned_version(kind, release, commit, hashes))
    return result


PINNED_VERSIONS = load_versions()


def client_cases():
    for kind, versions in PINNED_VERSIONS.items():
        for version in versions:
            for mode in (("main-post", "only") if kind == "cache" else ("main-post",)):
                yield kind + "/" + version["release"] + "/" + mode, kind, version, mode


def fetch_bundles(version):
    """Verify locally provisioned bundles; never fetch during tests."""
    paths = {}
    for name, spec in version["bundles"].items():
        path = VENDOR / version["directory"] / spec["filename"]
        if sha256_file(path) != spec["sha256"]:
            raise RuntimeError("pinned bundle hash mismatch: " + str(path))
        paths[name] = str(path)
    return paths

def redact(text):
    # The client masks the download URL via ::add-mask::; never echo it.
    lines = []
    for line in text.splitlines():
        if "add-mask" in line:
            lines.append("::add-mask::***REDACTED***")
        else:
            # Redact any loopback ticket query strings that may leak.
            line = re.sub(r"(ticket=)[^&\s]+", r"\1***", line)
            lines.append(line)
    return "\n".join(lines)

def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()

def check_tools(node_bin):
    missing = []
    if not (os.path.isfile(node_bin) and os.access(node_bin, os.X_OK)):
        missing.append("node (%s)" % node_bin)
    for tool in ("tar", "zstd", "unzstd", "zstdmt"):
        if shutil.which(tool) is None:
            missing.append(tool)
    return missing

def parse_kv_file(path):
    """Parse GITHUB_OUTPUT / GITHUB_STATE file-command format."""
    result = {}
    if not os.path.exists(path):
        return result
    with open(path) as f:
        content = f.read()
    lines = content.splitlines()
    i = 0
    while i < len(lines):
        m = re.match(r"^([^=<>]+)<<(.+)$", lines[i])
        if m:
            key, delim = m.group(1), m.group(2)
            vals = []
            i += 1
            while i < len(lines) and lines[i] != delim:
                vals.append(lines[i])
                i += 1
            result[key] = "\n".join(vals)
        elif "=" in lines[i]:
            key, _, val = lines[i].partition("=")
            result[key] = val
        i += 1
    return result

def file_tree_hashes(root):
    out = {}
    for dirpath, _, filenames in os.walk(root):
        for name in filenames:
            full = os.path.join(dirpath, name)
            rel = os.path.relpath(full, root)
            out[rel] = sha256_file(full)
    return out

class Client:
    def __init__(self, bundles, node_bin, service_url):
        self.bundles = bundles
        self.node_bin = node_bin
        self.service_url = service_url

    def run_action(self, bundle, inputs, token, workspace,
                   extra_env=None, timeout=300):
        """Run one dist bundle with runner-like env. Returns (rc, output)."""
        sidecar = os.path.normpath(os.path.join(workspace, "..", "sidecar"))
        os.makedirs(sidecar, exist_ok=True)
        out_file = os.path.join(sidecar, "github_output.txt")
        state_file = os.path.join(sidecar, "github_state.txt")
        # The runner pre-creates these files; @actions/core errors if
        # GITHUB_OUTPUT does not exist.
        for p in (out_file, state_file):
            with open(p, "w"):
                pass
        summary = os.path.join(workspace, "..", "github_summary.md")
        open(summary, "a").close()
        env = dict(os.environ)
        env["ACTIONS_CACHE_URL"] = self.service_url
        env["ACTIONS_RUNTIME_TOKEN"] = token
        env.pop("ACTIONS_CACHE_SERVICE_V2", None)
        env.pop("ACTIONS_CACHE_MODE", None)
        env.pop("ACTIONS_RESULTS_URL", None)  # artifact inputs untouched
        env["GITHUB_REF"] = "refs/heads/main"
        env["GITHUB_EVENT_NAME"] = "push"
        env["GITHUB_SERVER_URL"] = "https://github.com"
        env["GITHUB_WORKSPACE"] = workspace
        env["GITHUB_OUTPUT"] = out_file
        env["GITHUB_STATE"] = state_file
        env["GITHUB_STEP_SUMMARY"] = summary
        env["RUNNER_TEMP"] = os.path.join(sidecar, "runner_temp")
        env["RUNNER_TOOL_CACHE"] = os.path.join(sidecar, "runner_tools")
        os.makedirs(env["RUNNER_TEMP"], exist_ok=True)
        os.makedirs(env["RUNNER_TOOL_CACHE"], exist_ok=True)
        for key, value in inputs.items():
            env["INPUT_" + key.upper().replace(" ", "_")] = value
        if extra_env:
            env.update(extra_env)
        proc = subprocess.Popen(
            [self.node_bin, self.bundles[bundle]],
            cwd=workspace, env=env, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, text=True, start_new_session=True,
        )
        try:
            stdout, stderr = proc.communicate(timeout=timeout)
        except subprocess.TimeoutExpired:
            os.killpg(proc.pid, signal.SIGKILL)
            proc.communicate(timeout=5)
            raise
        output = redact(stdout + stderr)
        return (proc.returncode, output,
                parse_kv_file(out_file), parse_kv_file(state_file))

def yarn_probe(workdir):
    """Run real yarn --version and yarn cache dir under YARN_CACHE_FOLDER."""
    yarn_cache_root = os.path.join(workdir, "yarn-cache")
    os.makedirs(yarn_cache_root, exist_ok=True)
    os.environ["YARN_CACHE_FOLDER"] = yarn_cache_root
    ver = subprocess.run(
        [YARN_BIN, "--version"], capture_output=True, text=True, timeout=120,
    )
    if ver.returncode != 0:
        raise RuntimeError("yarn --version failed: %s" % ver.stderr)
    version = ver.stdout.strip()
    out = subprocess.run(
        [YARN_BIN, "cache", "dir"], capture_output=True, text=True,
        timeout=120, cwd=workdir,
    )
    if out.returncode != 0:
        raise RuntimeError("yarn cache dir failed: %s" % out.stderr)
    cache_dir = out.stdout.strip()
    return version, yarn_cache_root, cache_dir
