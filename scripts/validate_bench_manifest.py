#!/usr/bin/env python3
"""Check that a runner image's baked bench is really the bench its manifest describes.

Run by scripts/seal-runner-image.sh before an image is published, and independently of
the recipe that wrote the manifest. A claim is checked against the filesystem rather
than believed: an image whose bench disagrees with its manifest would send every job
down a fast path that tests code the image does not contain.

The expected key set is exact, as in the container-image guard: an unexpected key is the
only way an extra field could ride into a shared artifact.
"""
import hashlib
import json
import os
import pwd
import re
import subprocess
import sys
from pathlib import Path


def validate(manifest_path):
    """Return None when the manifest describes the bench, or raise SystemExit."""
    path = Path(manifest_path)

    def fail(message):
        print(f"{path}: {message}", file=sys.stderr)
        raise SystemExit(1)

    try:
        manifest = json.loads(path.read_text())
    except (OSError, ValueError) as error:
        fail(f"is not readable JSON: {error}")

    if not isinstance(manifest, dict):
        fail("is not a JSON object.")

    # An exact key set, for the reason the container guard uses one: an unexpected key is
    # the only way an extra field could ride into a shared artifact.
    expected = {'archive', 'bench_root', 'frappe_commit', 'frappe_ref', 'frappe_repository',
                'node_version', 'python_version', 'size_mib'}
    if set(manifest) != expected:
        fail(f"declares keys {sorted(manifest)}, expected {sorted(expected)}.")

    for key, pattern in (('frappe_commit', r'[0-9a-f]{40}'),
                         ('frappe_ref', r'[0-9a-f]{40}'),
                         ('frappe_repository', r'https://github\.com/[A-Za-z0-9._-]+/[A-Za-z0-9._-]+'),
                         ('node_version', r'[0-9]+\.[0-9]+\.[0-9]+'),
                         ('python_version', r'[0-9]+\.[0-9]+\.[0-9]+')):
        if not isinstance(manifest[key], str) or not re.fullmatch(pattern, manifest[key]):
            fail(f"has {key}={manifest[key]!r}, which is not what this sealer accepts.")

    sizes = manifest['size_mib']
    if not isinstance(sizes, dict) or set(sizes) != {'app', 'assets', 'env', 'node_modules'}:
        fail(f"declares size_mib={sorted(sizes) if isinstance(sizes, dict) else sizes!r}, "
             "expected app, assets, env and node_modules.")
    for key, value in sizes.items():
        if not isinstance(value, int) or value <= 0:
            fail(f"declares size_mib.{key}={value!r}, which is not a positive size.")

    # The archive is what a job restores from when its own workspace has been cleared, so
    # the copy this seal vouches for has to be the copy a job will read: the recorded
    # digest is checked against the bytes on disk rather than trusted.
    archive = manifest['archive']
    if not isinstance(archive, dict) or set(archive) != {'path', 'sha256', 'bytes'}:
        fail(f"declares archive={sorted(archive) if isinstance(archive, dict) else archive!r}, "
             "expected path, sha256 and bytes.")
    if not re.fullmatch(r'[0-9a-f]{64}', archive['sha256']):
        fail(f"declares an archive digest {archive['sha256']!r}, which is not a sha256.")
    if not isinstance(archive['bytes'], int) or archive['bytes'] <= 0:
        fail(f"declares archive bytes={archive['bytes']!r}, which is not a size.")
    archive_path = Path(archive['path'])
    if not archive_path.is_file():
        fail(f"names an archive {archive_path} that is not a file.")
    if archive_path.stat().st_size != archive['bytes']:
        fail(f"records the archive at {archive['bytes']} bytes, but it is "
             f"{archive_path.stat().st_size}.")
    digest = hashlib.sha256()
    with archive_path.open('rb') as stream:
        for block in iter(lambda: stream.read(4 * 1024 * 1024), b''):
            digest.update(block)
    if digest.hexdigest() != archive['sha256']:
        fail(f"records the archive at {archive['sha256']}, but it hashes to {digest.hexdigest()}.")

    root = Path(manifest['bench_root'])
    if not root.is_dir():
        fail(f"names a bench root {root} that is not a directory.")

    app = root / 'apps' / 'frappe'
    try:
        if os.geteuid() == 0:
            # Git refuses to read a repository owned by another account, and sealing runs as
            # root over a checkout the runner owns, so the query is made as the owner rather
            # than with a blanket safe.directory, which would silence that check everywhere
            # instead of for this one read. runuser rejects a bare uid, so the account is
            # named first.
            owner = pwd.getpwuid(app.stat().st_uid).pw_name
            query = ['sudo', '-u', owner, '--', 'git', '-C', str(app), 'rev-parse', 'HEAD']
        else:
            query = ['git', '-C', str(app), 'rev-parse', 'HEAD']
        head = subprocess.run(query, capture_output=True, text=True, check=True).stdout.strip()
    except (OSError, KeyError, subprocess.CalledProcessError):
        fail(f"cannot read the baked app's commit from {app}.")
    if head != manifest['frappe_commit']:
        fail(f"claims the app at {manifest['frappe_commit']}, but the bench carries {head}.")

    # Every directory the bake promised is measured against what it recorded. A job that
    # finds a missing virtualenv or an unbuilt asset tree fails in the middle of a test
    # run, which is a far worse place to learn that the image was incomplete.
    def measure(relative):
        # Named target, not path: fail() reads path to name the manifest in its
        # message, and shadowing it here would report the wrong file.
        target = root / relative
        if not target.exists():
            fail(f"records a size for {relative}, but {target} does not exist.")
        total = subprocess.run(['du', '-sm', str(target)], capture_output=True, text=True,
                               check=True).stdout.split()[0]
        return int(total)

    for relative, key in (('apps/frappe', 'app'), ('env', 'env'),
                          ('apps/frappe/node_modules', 'node_modules'),
                          ('sites/assets', 'assets')):
        recorded = sizes[key]
        # du counts whole MiB and block rounding moves either way, so the check is a
        # tolerance rather than equality: it is here to catch a truncated or absent tree,
        # not to re-measure something the bake already measured.
        actual = measure(relative)
        if abs(actual - recorded) > max(64, recorded // 4):
            fail(f"records {key} at {recorded} MiB, but {relative} measures {actual} MiB.")

    # node_modules is measured inside apps/frappe, so summing the four records would count
    # it twice and report a size the bench does not occupy.
    total = sizes['app'] + sizes['env'] + sizes['assets']
    print(f"Bench manifest agrees with the image: {manifest['frappe_repository']} at "
          f"{manifest['frappe_commit'][:12]}, {total} MiB recorded, "
          f"archive {archive['bytes'] // (1024 * 1024)} MiB.")


if __name__ == "__main__":
    if len(sys.argv) != 2:
        print(f"usage: {sys.argv[0]} MANIFEST", file=sys.stderr)
        raise SystemExit(2)
    validate(Path(sys.argv[1]))
