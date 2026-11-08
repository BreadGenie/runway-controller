"""Offline pinned action clients shared by the relay integration test."""
import hashlib
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess

ROOT = Path(__file__).resolve().parents[2]
VENDOR = ROOT / '.scratch/runway-regional-cache/vendor'
NODE_DEFAULT = shutil.which('node') or os.path.expanduser('~/.local/opt/node/bin/node')
YARN_BIN = os.environ.get('YARN_BIN') or shutil.which('yarn') or ''


def pinned_version(kind, release, commit, hashes):
    """Describe full-commit upstream files without performing network I/O."""
    bundles = {}
    for name, digest in hashes.items():
        path = "action.yml" if name == "action.yml" else "dist/" + name + "/index.js"
        bundles[name] = {
            "url": "https://raw.githubusercontent.com/actions/" + kind + "/" + commit + "/" + path,
            "sha256": digest,
            "filename": "action.yml" if name == "action.yml" else name + "-index.js",
        }
    prefix = "actions-cache" if kind == "cache" else kind
    return {"release": release, "commit": commit,
            "directory": prefix + "-" + commit[:8], "bundles": bundles}


PINNED_VERSIONS = {
    "cache": [
        pinned_version("cache", "v4.3.0", "0057852bfaa89a56745cba8c7296529d2fc39830", {
            "restore": "d7b9163d46eb2a31a2ba58c40fa8a8c342d19b9211676ca6a32809d298352475",
            "save": "5274b49a495e746792cb5f50a6068f780eb5080b8510a9dbbd905e350d8d8f07",
            "restore-only": "f740b8f77a9eeb0223d54b55e14df240e7ad6338e9648730d8e4551cf78ae172",
            "save-only": "7f64677407a4befe523854fb28794faa4e2d344dfed5d6cd4887fa9ae5428b3f",
        }),
        pinned_version("cache", "v5.1.0", "caa296126883cff596d87d8935842f9db880ef25", {
            "restore": "a49301428b4c5477522c00f6464566457ed327d64ae5f80f8c12e762fb2ea9a2",
            "save": "b969065795aaf5c6d20ba158e910be5d353eefb02eb2785263eb32e13090c066",
            "restore-only": "6255afaa3956351b8cfefc1e82f026b4db418a10678a8eaddf3d6c55f81744de",
            "save-only": "7fb63f90f06ce6a10f39d40a113f99791bffdedad5394cdad5b4dfaa644559cb",
        }),
        pinned_version("cache", "v6.1.0", "55cc8345863c7cc4c66a329aec7e433d2d1c52a9", {
            "restore": "79a05f72974c12e3796bf09c897fce453a855f5f4a4abc980be6f671c545954d",
            "save": "9193aa9dbe5025f2bf39802ae241470af35c5e5737fd4bec6e1d6dbaad153bb4",
            "restore-only": "8522c6be9a4650a3b1571f7222293f262cc4c4708fbe061d8ed04654d476cf16",
            "save-only": "0d3cf341a79cdc0a8ce5a71103f46174e12cf4f98db46f15b8e604932eefd487",
        }),
    ],
    "setup-node": [
        pinned_version("setup-node", "v6.5.0", "249970729cb0ef3589644e2896645e5dc5ba9c38", {
            "setup": "842178c4df5924b2a7061efec35f9d7c7e3c7d84f49bc561689786c5c6fb2720",
            "cache-save": "b5876872e7ca73333940c70f4cc50be6cef4d6021e72612f577575a97df8701d",
            "action.yml": "ad45f1922115116fb4706651d0a9262332bc4e4f11eb2021e09789dd1af18085",
        }),
        pinned_version("setup-node", "v7.1.0", "949feb2413d6458794dcd2491c4babbbce0c15c1", {
            "setup": "61462d80fd9850ceb0fd072b80777ce297f537fb2ec2d7c8d87983e80ce7e4fa",
            "cache-save": "207a598f869d4f64eeb5a3d9e1e847472bba56120b7b12c40f35900d53fd58e4",
            "action.yml": "84cd55863dbadfb83ed786c0bb4336553c2f4ceb52e102fccec01848f1977387",
        }),
    ],
}


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
