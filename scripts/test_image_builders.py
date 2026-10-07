"""Exercise the image-builder guards without a builder guest.

The three builders under test are shell scripts that act on absolute guest paths:
``/etc``, ``/home/runner``, ``/var/lib/docker``, and an exported filesystem. Running
them for real would mean touching the host, so each test runs a rewritten copy of a
script against a private temporary root and a set of mocked binaries.

That rewriting is the load-bearing part of this harness, so it is deliberately
strict. ``route`` replaces only the absolute paths listed in ``GUEST_PATHS``, and
``assert_rewritten`` fails on any absolute path the harness does not know about. If a
builder grows a new one, a test breaks and a human has to decide where that path
belongs in the test tree. Without that rule, a path added later would quietly be
followed onto the real host by a test that believed it was isolated.
"""

import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
GUEST = HERE.parent / 'guest'
PREPARE = GUEST / 'prepare-frappe-container-images.sh'
SEAL = HERE / 'seal-runner-image.sh'
BAKE = GUEST / 'prepare-frappe-bench.sh'
FINALIZE = HERE / 'finalize-runner-image.sh'

BUILDER_FLAG = '--inside-unregistered-image-builder'
PRESERVE_FLAG = '--preserve-container-images'

# The reviewed digests, restated here so a test states its expectation instead of
# reading it back out of the script under test.
POSTGRES = 'postgres:18.0@sha256:41fc5342eefba6cc2ccda736aaf034bbbb7c3df0fdb81516eba1ba33f360162c'
MARIADB = 'mariadb:11.8@sha256:6422478cb8e159f080fb1d8ccf65101e26fe51385787fde7d16c3b165a331f15'
SMTP4DEV = 'rnwood/smtp4dev:3.7.1@sha256:9356c41b77175aa9dd530a8e1ded29cb2574f2d2474c2b0ab94f202763e6feb9'
SERVICE_IMAGES = (POSTGRES, MARIADB, SMTP4DEV)

# Docker records a RepoDigest without the tag. The mock emits exactly that shape, so a
# builder that compares against the tagged pinned reference fails here rather than
# passing by accident against a mock shaped like its own expectation.
# The digest a daemon records for an image, as `docker image inspect` prints it: the
# repository and digest with no tag, so `rnwood/smtp4dev` keeps its namespace. Keyed by
# the full pinned reference the builders ask about. A builder that compared the tagged
# pinned reference against this would find nothing, which is the point of the stub
# answering in the daemon's own form.
REPO_DIGESTS = {reference: reference.split('@')[0].rsplit(':', 1)[0] + '@' + reference.split('@')[1]
                for reference in SERVICE_IMAGES}
IMAGE_IDS = {reference: f'sha256:{index:064x}' for index, reference in enumerate(SERVICE_IMAGES, 1)}

# Absolute guest paths a builder may mention, longest first so a prefix never shadows
# a longer path during substitution.
GUEST_PATHS = (
    # The work folder the controller asks a Runway JIT runner for, which is where the
    # bake builds and where a job looks. It has to be owned here for the same reason
    # actions-runner/_work is: the bake writes it, so a test that left it unrouted would
    # write to the host running the suite.
    '/home/runner/work',
    '/opt/runway',
    # The profile's toolchain. The bake refuses to start without these three, so a test
    # that reaches a later guard has to provide them, and providing them unrouted would
    # write to whatever python3 the machine running the tests happens to have.
    '/opt/hostedtoolcache',
    '/usr/local/bin',
    '/home/runner/actions-runner/_work',
    '/home/runner/actions-runner/_diag',
    '/home/runner/actions-runner/.credentials_rsaparams',
    '/home/runner/actions-runner/.credentials',
    '/home/runner/actions-runner/.docker/config.json',
    '/home/runner/actions-runner/.runner',
    '/home/runner/actions-runner',
    '/var/lib/systemd/random-seed',
    '/var/lib/dbus/machine-id',
    '/var/lib/apt/lists',
    '/var/lib/runway/bootstrap.json',
    '/var/lib/containerd',
    '/var/lib/docker',
    '/etc/runway-runner-image.txt',
    '/etc/runway-container-images.json',
    '/etc/runway-frappe-bench.json',
    '/etc/machine-id',
    '/etc/ssh',
    '/etc/hostname',
    '/etc/hosts',
    '/etc/os-release',
    '/root/.docker/config.json',
    '/root/.docker',
    '/root/.cache',
    '/root/.ssh',
    '/home/runner/.docker/config.json',
    '/home/runner/.docker',
    '/home/runner/.cache',
    '/home/runner/.ssh',
    '/home/runner',
    '/home/ubuntu',
    # The home directories themselves: the sealer loops over them for `.ssh`, `.cache`
    # and `.docker`, so the bare paths have to be owned as well as the entries in them.
    '/root',
    '/var/log',
    '/var/tmp',
    '/tmp',
)

# Names a builder may use without the harness owning them. A repository URL is here for
# the same reason as the runner's bin: it is a name the recipe reads out of a manifest and
# hands to git, never a path on this machine, and the self-check cannot tell a URL from a
# directory. `/..` is the relative hop the bake takes to reach a script it ships beside
# itself; routing it would turn the hop into the private root and break the very lookup it
# is meant to preserve.
READ_ONLY_PREFIXES = ('/home/runner/actions-runner/bin', '/github.com', '/..')

ABSOLUTE = re.compile(r'(?<![\w.~$-])/(?:[A-Za-z0-9_.+-]+/?)+')
KNOWN_PREFIXES = tuple(GUEST_PATHS) + READ_ONLY_PREFIXES

DOCKER_STUB = '''#!/usr/bin/env python3
"""A scripted Docker CLI: answers only what the builders ask, and records each call."""
import json, os, sys

IMAGES = @IMAGES@
REPO_DIGESTS = @DIGESTS@
IMAGE_IDS = @IDS@
LOG = @LOG@
STATE = @STATE@


def record(*argv):
    with open(LOG, 'a') as handle:
        handle.write(' '.join(argv) + chr(10))


def blank():
    return {'containers': [], 'volumes': [], 'dangling': [],
            'networks': ['bridge', 'host', 'none'],
            'tags': [], 'repo_digests': {}, 'image_ids': {}, 'data_root': '/var/lib/docker'}


def load():
    if os.path.exists(STATE):
        return json.load(open(STATE))
    return blank()


def save(state):
    json.dump(state, open(STATE, 'w'))


def match(state, argument):
    """Resolve a reference the way the CLI does: pinned form, or tag form if tagged.

    A tag form only resolves when the tag is present in the store. That is what makes
    `docker tag` necessary rather than decorative: without it a job pulling by tag would
    miss the baked layers, and a stub that always resolved tags would not notice.
    """
    if argument in state['repo_digests']:
        return argument
    for pinned in IMAGES:
        tag = pinned.split('@')[0]
        if argument == tag and tag in state['tags']:
            return pinned
    return None


def seed(state):
    for pinned in IMAGES:
        state['repo_digests'][pinned] = REPO_DIGESTS[pinned]
        state['image_ids'][pinned] = IMAGE_IDS[pinned]
    return state


def pull(state, arguments):
    reference = arguments[0]
    # A pinned reference resolves whether or not the tag is present. The pull records the
    # image's digest and identity; it does not apply the workflow's tag. That is what
    # `docker tag` is for, and a stub where the pull also tagged would make the tag step
    # look like decoration.
    pinned = reference if reference in IMAGES else None
    if pinned is None:
        print('Error response from daemon: manifest unknown', file=sys.stderr)
        return 1
    # A pull records what the registry served, which is the pinned digest unless a test
    # says otherwise. `served` models a registry that resolves the reference to different
    # content, which is the case the digest comparison exists to catch: the pull succeeds
    # and only the recorded digest reveals what actually arrived.
    state['repo_digests'][pinned] = state.get('served', {}).get(pinned, REPO_DIGESTS[pinned])
    state['image_ids'][pinned] = IMAGE_IDS[pinned]
    print('Status: Downloaded newer image for ' + pinned.split('@')[0])
    return 0


def ls(state, arguments):
    if 'dangling=true' in arguments:
        print(chr(10).join(state['dangling']))
        return 0
    template = None
    for position, argument in enumerate(arguments):
        if argument == '--format':
            template = arguments[position + 1]
    # Without --no-trunc Docker prints a 12-character short id. The stub reproduces that
    # so a builder comparing a listing against an inspect without the flag fails here,
    # rather than passing against a mock that always answers in full.
    truncated = '--no-trunc' not in arguments
    rows = []
    # Enumerate the store's own records, not the three reviewed images: an extra image
    # has to appear in this listing, otherwise the exactness checks cannot see it.
    for pinned in state['repo_digests']:
        tag = pinned.split('@')[0]
        if tag not in state['tags']:
            continue
        identifier = state['image_ids'].get(pinned, '')
        if truncated:
            identifier = identifier.replace('sha256:', '')[:12]
        if '{{.Repository}}:{{.Tag}}' == template:
            rows.append(tag)
        elif '{{.ID}}' == template:
            rows.append(identifier)
        else:
            rows.append('')
    print(chr(10).join(rows))
    return 0


def inspect(state, arguments):
    # --format and its value are flags; whatever remains is the reference.
    reference = positional(arguments)
    reference = reference[0] if reference else ''
    pinned = match(state, reference)
    if pinned is None:
        # A pinned reference resolves from the store's digest record alone, whether or
        # not the tag was applied. A bare tag resolves only once it is tagged, which is
        # what makes `docker tag` load-bearing rather than decorative.
        for candidate in IMAGES:
            if reference == candidate:
                pinned = candidate
                break
    if pinned is None:
        print('Error response from daemon: No such image: ' + reference, file=sys.stderr)
        return 1
    template = ''
    for position, argument in enumerate(arguments):
        if argument == '--format':
            template = arguments[position + 1]
    # The store holds whatever the pulls recorded. A test that corrupts the record has to
    # see that corruption here, so these reads never fall back to the constant.
    if 'RepoDigests' in template:
        # What a daemon records is the repository and digest with no tag, which is what
        # the builders have to canonicalise a tagged reference into.
        print(state['repo_digests'].get(pinned, ''))
    elif template == '{{.Id}}':
        print(state['image_ids'].get(pinned, ''))
    else:
        print('[]')
    return 0


def positional(arguments):
    """The reference argument: skip every flag and the value that follows it."""
    result = []
    skip = False
    for argument in arguments:
        if skip:
            skip = False
            continue
        if argument.startswith('--'):
            skip = argument in ('--format', '--filter')
            continue
        result.append(argument)
    return result or ['']


def main(argv):
    record('docker', *argv)
    if not argv:
        return 1
    state = load()
    command, arguments = argv[0], argv[1:]
    if command == 'pull':
        code = pull(state, arguments)
    elif command == 'tag':
        state['tags'].append(arguments[1])
        code = 0
    elif command == 'info':
        # The daemon reports the path it was started with, which is the routed one in
        # this guest. Answering the same path the recipe asks about would let a real
        # data-root mismatch pass.
        print(state['data_root'])
        code = 0
    elif command == 'ps':
        print(chr(10).join(state['containers']))
        code = 0
    elif command == 'volume' and arguments[:1] == ['ls']:
        print(chr(10).join(state['volumes']))
        code = 0
    elif command == 'network' and arguments[:1] == ['ls']:
        print(chr(10).join(state['networks']))
        code = 0
    elif command == 'image' and arguments[:1] == ['ls']:
        code = ls(state, arguments[1:])
    elif command == 'image' and arguments[:1] == ['inspect']:
        # The raw arguments reach inspect, because it needs both the --format template and the
        # reference. Passing positional() here would strip --format and its value, and
        # inspect would answer every template with '[]'.
        code = inspect(state, arguments[1:])
    elif command == 'builder':
        code = 0
    else:
        code = 0
    save(state)
    return code


sys.exit(main(sys.argv[1:]))
'''


def routed_paths(text):
    """Every absolute path a script body mentions, for the harness self-check."""
    found = set()
    for line in text.splitlines():
        stripped = line.lstrip()
        if stripped.startswith('#'):
            continue
        for match in ABSOLUTE.finditer(line):
            candidate = match.group(0).rstrip('/')
            # sbin entries appear only in the PATH the finalizer prepends for its own
            # tools; no builder reads or writes a file there.
            if candidate.startswith(('/usr/', '/dev/', '/sbin', '/usr/sbin', '/bin', '/proc', '/sys')):
                continue
            found.add(candidate)
    return found


# Longest first, so `/root/.docker` is never rewritten as `/root` plus a leftover.
_ROUTE_PATTERN = re.compile('|'.join(re.escape(path) for path in sorted(GUEST_PATHS, key=len, reverse=True)))


def route_text(text, root):
    """Rewrite every guest path in one pass.

    A single substitution matters: replacing `/home/runner/actions-runner` and then
    `/home/runner` separately would rewrite the already-substituted text a second time
    and produce a doubled prefix.
    """
    return _ROUTE_PATTERN.sub(lambda match: root + match.group(0), text)


class Harness(unittest.TestCase):
    """A private guest root, mocked binaries, and rewritten copies of the builders."""

    maxDiff = None

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp, True)
        self.root = str(self.tmp / 'guest')
        self.log = self.tmp / 'calls.log'
        self.state = self.tmp / 'state.json'
        self.bin = self.tmp / 'bin'
        self.bin.mkdir()
        self.state.write_text(json.dumps({
            'containers': [], 'volumes': [], 'dangling': [],
            'networks': ['bridge', 'host', 'none'], 'tags': [], 'repo_digests': {},
            'image_ids': {}, 'data_root': self.root + '/var/lib/docker', 'active': False,
            'starts': [], 'stops': []}))
        self.install_mocks()

    # -- mocked binaries --------------------------------------------------
    def mock_python(self, name, body):
        path = self.bin / name
        # An absolute interpreter, not `env python3`: the mocked PATH resolves
        # `python3` to this harness's own logging wrapper, which would swallow the
        # stub and record calls under the wrong name.
        path.write_text(f'#!{sys.executable}\n' + body)
        path.chmod(0o755)

    def mock_shell(self, name, body):
        path = self.bin / name
        path.write_text('#!/usr/bin/env bash\nset -eu\n' + body)
        path.chmod(0o755)

    def mock_raw(self, name, body):
        """A mock with its own shebang, for stubs that are not bash."""
        path = self.bin / name
        path.write_text(body.replace('SHEBANG', f'#!{sys.executable}', 1)
                        if body.startswith('SHEBANG') else body)
        path.chmod(0o755)

    def install_mocks(self):
        # The stub is substituted with str.replace rather than str.format: its body has
        # braces and format specifiers of its own, which .format would try to expand.
        self.mock_python('docker', DOCKER_STUB
                         .replace('@IMAGES@', repr(list(SERVICE_IMAGES)))
                         .replace('@DIGESTS@', repr(REPO_DIGESTS))
                         .replace('@IDS@', repr(IMAGE_IDS))
                         .replace('@LOG@', repr(str(self.log)))
                         .replace('@STATE@', repr(str(self.state))))
        self.mock_shell('systemctl', f'''
printf 'systemctl %s\\n' "$*" >> {str(self.log)!r}
python3 - {str(self.state)!r} "$@" <<'STATE'
import json, os, sys
path, argv = sys.argv[1], sys.argv[2:]
state = json.load(open(path))
action = argv[0] if argv else ''
if action == 'is-active':
    sys.exit(0 if state.get('active') else 1)
if action == 'start':
    state.setdefault('starts', []).extend(argv[1:])
if action == 'stop':
    state.setdefault('stops', []).extend(argv[1:])
    state['active'] = False
json.dump(state, open(path, 'w'))
STATE
''')
        for name in ('apt-get', 'cloud-init', 'sync'):
            self.mock_shell(name, f'printf "{name} %s\\n" "$*" >> {str(self.log)!r}\n')
        # `du -sb` is asked about each data root separately and the recipe adds the answers, so
        # the mock answers per path. These are the measured builder's numbers: Docker's
        # root holds little under the containerd image store, and the images live in
        # containerd.
        self.mock_shell('du', f'''
case "$*" in
  *containerd*) printf '1503238553\\t{self.root}/var/lib/containerd\\n' ;;
  *) printf '193536\\t{self.root}/var/lib/docker\\n' ;;
esac
''')
        self.mock_shell('df', "printf 'Filesystem Size Used Avail Use%%\\n'\n")
        # find, ln and truncate still act on the private tree; they only get logged.
        for name in ('find', 'ln', 'truncate', 'rm', 'chmod', 'date'):
            self.mock_shell(name, f'''
printf '{name} %s\\n' "$*" >> {str(self.log)!r}
exec /usr/bin/{name} "$@"
''')
        # `id runner` must succeed for the recipe to proceed. The real id would say this
        # guest has no runner user, which is a property of the machine rather than of
        # the code under test.
        self.mock_shell('id', f'''
case "${{1:-}}" in
  runner) printf 'uid=1001(runner) gid=1001(runner) groups=1001(runner)\\n'; exit 0 ;;
esac
exec /usr/bin/id "$@"
''')
        self.mock_shell('python3', f'''
printf 'python3 %s\\n' "$*" >> {str(self.log)!r}
exec {sys.executable!r} "$@"
''')

    def install_fs_tools(self, blocks, block_size, resized_blocks=None):
        """Mock e2fsprogs, debugfs, zerofree and zstd over the private export.

        `debugfs` reports capacity the way the real tool does, which is the point of
        these tests: a filesystem larger than the published target is normal here, so
        any guard that read capacity as content size would refuse every real export.

        `resized_blocks` overrides what `debugfs` reports once resize2fs has run. Left
        unset, the filesystem reports the target resize2fs was given, which is what a
        successful resize produces.
        """
        size_state = self.tmp / 'fs_size'
        size_state.write_text(json.dumps({'blocks': blocks, 'resized': resized_blocks,
                                          'block_size': block_size}))
        for tool in ('e2fsck', 'resize2fs', 'tune2fs', 'zerofree', 'zstd', 'sha256sum'):
            self.mock_shell(tool, f'printf "{tool} %s\\n" "$*" >> {str(self.log)!r}\n')
        self.target_gib = 0
        self.mock_raw('debugfs', f'''SHEBANG
import json, os, re, subprocess, sys

SIZE = {str(size_state)!r}
LOG = {str(self.log)!r}
command = sys.argv[sys.argv.index('-R') + 1]
with open(LOG, 'a') as handle:
    handle.write('debugfs ' + command + chr(10))
state = json.load(open(SIZE))
# A successful resize2fs is what makes the filesystem report its new capacity. The log
# records each call as it happens, so a resize line already written means the resize has
# happened. Before it, the filesystem still reports the builder's original size.
logged = open(LOG).read().splitlines()
resize_lines = [line for line in logged if line.startswith('resize2fs ')]
if command == 'stats':
    if resize_lines:
        if state.get('resized') is not None:
            # An explicit override models a resize that did not actually take.
            blocks = state['resized']
        else:
            # A resize that took: the filesystem is now the target it was given.
            target = None
            for line in resize_lines:
                found = re.search(r'(\\d+)G$', line)
                if found:
                    target = int(found.group(1))
            blocks = (target * (1024 ** 3) // state['block_size']) if target else state['blocks']
    else:
        blocks = state['blocks']
    print('Filesystem volume name:  <none>')
    print('Block size:              ' + str(state['block_size']))
    print('Block count:             ' + str(blocks))
    print('Inode count:             1000')
    sys.exit(0)
if command.startswith('stat '):
    path = command.split(' ', 1)[1]
    # Only the sealing marker is present, as it is in a properly sealed export. An
    # absent path must print nothing at all, the way real debugfs reports one: the
    # finalizer's guard greps for '^Inode:', so printing an 'Inode: 0' line here would
    # make every absent credential look present.
    # The finalizer's stat calls arrive routed, so match the marker by its tail.
    if path.endswith('/etc/runway-runner-image.txt'):
        print('Inode: 12  Mode:  0100644  Regular:  0  Type: 1')
    sys.exit(0)
sys.exit(0)
''')

    # -- guest fixtures ---------------------------------------------------
    def fresh_guest(self):
        """The guest as install-guest-runner.sh leaves it, with images already baked."""
        runner = Path(self.root + '/home/runner/actions-runner')
        (runner / 'bin').mkdir(parents=True)
        (runner / 'bin' / 'Runner.Listener').write_text('')
        (runner / 'bin' / 'Runner.Listener').chmod(0o755)
        (runner / '_work').mkdir()
        (runner / '_diag').mkdir()
        for path in ('/etc', '/var/log', '/tmp', '/var/tmp', '/var/lib/docker',
                     '/var/lib/dbus', '/var/lib/apt/lists', '/var/lib/systemd', '/etc/ssh'):
            Path(self.root + path).mkdir(parents=True, exist_ok=True)
        Path(self.root + '/etc/runway-runner-image.txt').write_text('OS=Ubuntu 24.04\n')
        Path(self.root + '/etc/machine-id').write_text('a' * 32 + '\n')
        Path(self.root + '/etc/os-release').write_text('ID=ubuntu\nVERSION_ID="24.04"\n')
        Path(self.root + '/etc/ssh/ssh_host_ed25519_key').write_text('host key\n')
        Path(self.root + '/var/log/journal.log').write_text('log line\n')
        # Docker 29 uses the containerd image store, so both roots exist in the guest
        # the sealer will be pointed at.
        Path(self.root + '/var/lib/containerd').mkdir(parents=True, exist_ok=True)
        self.write_manifest()
        self.seed_store()

    def seed_store(self, tagged=True):
        """Put the three pinned images into the mocked store, as a finished bake does.

        A real pull records the image's digest immediately; `docker tag` is what makes the
        workflow's tag resolve. Seeding them separately matters: a mock where both arrive
        together cannot tell whether the tag step is actually required.
        """
        state = self.docker_state()
        state['repo_digests'] = dict(REPO_DIGESTS)
        state['image_ids'] = dict(IMAGE_IDS)
        state['tags'] = [reference.split('@')[0] for reference in SERVICE_IMAGES] if tagged else []
        self.save_state(state)

    def add_image(self, tag, identifier):
        """Record a tagged image the store holds but the recipe never pulled."""
        state = self.docker_state()
        state['repo_digests'][f'{tag}@sha256:' + 'a' * 64] = f'{tag}@sha256:' + 'a' * 64
        state['image_ids'][f'{tag}@sha256:' + 'a' * 64] = identifier
        state['tags'].append(tag)
        self.save_state(state)

    def pull_all(self):
        """Deprecated alias kept out of use: fixtures call seed_store()."""
        raise AssertionError('use seed_store')

    def write_manifest(self, **overrides):
        images = []
        for reference in SERVICE_IMAGES:
            tag = reference.split('@')[0]
            name, _, version = tag.rpartition(':')
            entry = {'reference': reference, 'repository': name, 'tag': version,
                     'amd64_image_id': IMAGE_IDS[reference]}
            entry.update(overrides.pop(reference, {}))
            images.append(entry)
        # Sizes modelled on the measured builder: Docker's data root holds almost nothing
        # under the containerd image store, and the images live in containerd.
        docker_bytes, containerd_bytes = 193536, 1503238553
        by_root = {'docker': docker_bytes, 'containerd': containerd_bytes}
        document = {'schema': 'runway.container-images/v1',
                    'created_utc': '2026-01-01T00:00:00Z',
                    # The guest paths are routed like every other one, because the
                    # manifest is compared against the sealer's own idea of the data
                    # roots and both have to name the same places.
                    'data_root': self.root + '/var/lib/docker',
                    'containerd_data_root': self.root + '/var/lib/containerd',
                    # store_bytes and total_bytes are the total across both roots, not
                    # either root on its own.
                    'store_bytes': sum(by_root.values()),
                    'store_bytes_by_root': by_root,
                    'source_registry': 'registry-1.docker.io',
                    'image_count': len(images), 'images': images,
                    'total_bytes': sum(by_root.values())}
        document.update(overrides)
        path = Path(self.root + '/etc/runway-container-images.json')
        path.write_text(json.dumps(document, indent=2) + '\n')
        return path

    def docker_state(self):
        return json.loads(self.state.read_text())

    def save_state(self, state=None):
        # Write the state it is given. Re-reading the file here would discard any
        # mutation the caller just made and silently undo every fixture.
        self.state.write_text(json.dumps(self.docker_state() if state is None else state))

    def calls(self, name=None):
        if not self.log.exists():
            return []
        lines = self.log.read_text().splitlines()
        return [line for line in lines if name is None or line.split(' ', 1)[0] == name]

    def touch(self, guest_path, content='credential\n'):
        path = Path(self.root + guest_path)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)

    # -- execution --------------------------------------------------------
    def route(self, source):
        """Refuse to run anything the harness cannot fully redirect at the real root.

        The self-check runs before the script is written, so an unrouted path stops the
        test instead of reaching the host it was written for. That ordering is the whole
        point of this harness: the builders act on absolute paths, so a test that runs
        them without complete isolation is not a test, it is an incident.
        """
        text = source.read_text()
        unrouted = {path for path in routed_paths(text)
                    if not path.startswith(KNOWN_PREFIXES)
                    and not any(path.startswith(prefix + '/') for prefix in KNOWN_PREFIXES)}
        # assertFalse rather than assertNotIn: a failure must name the offending path,
        # not print a whole builder script into the test log.
        self.assertFalse(unrouted, f'{source.name} names paths the harness does not own: {unrouted}')
        routed = route_text(text, self.root)
        self.assertNotIn(self.root + self.root, routed, 'a path was rewritten twice')
        # Remove every routed prefix, then require that no owned guest path survives in
        # host form. Comparing substrings directly would false-positive on the private
        # root itself, which the harness places under /tmp.
        # The sentinel must be a word character, otherwise the `/` after it still
        # matches as the start of an absolute path and the check reports routed text.
        residue = routed.replace(self.root, 'PRIVATEROOT')
        leftover = {path for path in routed_paths(residue) if path in set(GUEST_PATHS)}
        self.assertFalse(leftover, f'{source.name} still names host paths: {leftover}')
        return routed

    def run_script(self, source, arguments, *, euid=0, name='builder.sh', in_guest_dir=False):
        # in_guest_dir writes the script to <tmp>/guest/<name>, so a script that resolves a
        # helper at ../scripts/ finds one the harness placed at <tmp>/scripts/. Without it the
        # bake's archive checker resolves outside the private tree.
        routed = self.route(source).replace('$EUID', str(euid))
        path = Path(self.tmp / name) if not in_guest_dir else Path(self.tmp / 'guest' / name)
        path.write_text(routed)
        # The sealer resolves its validator relative to itself, so the harness places
        # it beside the routed script rather than leaving the sealer to find nothing.
        for sibling in ('validate_bench_manifest.py',):
            source = HERE / sibling
            if source.exists():
                (self.tmp / sibling).write_text(source.read_text())
        environment = {**os.environ, 'PATH': f'{self.bin}:{os.environ["PATH"]}'}
        environment.pop('EUID', None)
        return subprocess.run(['bash', str(path), *arguments], capture_output=True,
                              text=True, env=environment)


class PrepareRecipeTests(Harness):
    def setUp(self):
        super().setUp()
        Path(self.root + '/etc').mkdir(parents=True, exist_ok=True)
        Path(self.root + '/etc/os-release').write_text('ID=ubuntu\nVERSION_ID="24.04"\n')

    def run_prepare(self, *extra, euid=0):
        return self.run_script(PREPARE, [BUILDER_FLAG, *extra], euid=euid, name='prepare.sh')

    def guest_with_runner(self):
        runner = Path(self.root + '/home/runner/actions-runner')
        (runner / 'bin').mkdir(parents=True)
        (runner / 'bin' / 'Runner.Listener').write_text('')
        (runner / 'bin' / 'Runner.Listener').chmod(0o755)
        Path(self.root + '/etc/runway-runner-image.txt').write_text('OS=Ubuntu 24.04\n')
        Path(self.root + '/var/lib/docker').mkdir(parents=True, exist_ok=True)
        # Docker 29 keeps image content in containerd, so the recipe measures both roots.
        Path(self.root + '/var/lib/containerd').mkdir(parents=True, exist_ok=True)

    def test_pulls_pinned_digests_and_tags_workflow_references(self):
        self.guest_with_runner()
        result = self.run_prepare()
        self.assertEqual(result.returncode, 0, result.stderr)
        pulls = [line for line in self.calls('docker') if line.startswith('docker pull ')]
        self.assertEqual(pulls, [f'docker pull {reference}' for reference in SERVICE_IMAGES])
        tags = [line for line in self.calls('docker') if line.startswith('docker tag ')]
        self.assertEqual(tags, [f'docker tag {reference} {reference.split("@")[0]}'
                                for reference in SERVICE_IMAGES])

    def test_records_the_digests_this_run_observed(self):
        self.guest_with_runner()
        self.assertEqual(self.run_prepare().returncode, 0)
        manifest = json.loads(Path(self.root + '/etc/runway-container-images.json').read_text())
        self.assertEqual([entry['reference'] for entry in manifest['images']], list(SERVICE_IMAGES))
        # The amd64 identity comes from the store, so it is the id Docker reported.
        self.assertEqual([entry['amd64_image_id'] for entry in manifest['images']],
                         [IMAGE_IDS[reference] for reference in SERVICE_IMAGES])
        self.assertEqual(manifest['image_count'], 3)
        # The manifest names guest paths, so it carries the routed ones the guest had.
        self.assertEqual(manifest['data_root'], self.root + '/var/lib/docker')
        self.assertEqual(manifest['containerd_data_root'], self.root + '/var/lib/containerd')
        # store_bytes is the total across both roots, and the roots are not equal: with
        # the containerd image store almost everything is in containerd.
        self.assertEqual(manifest['store_bytes'],
                         sum(manifest['store_bytes_by_root'].values()))
        self.assertEqual(manifest['total_bytes'], manifest['store_bytes'])
        self.assertLess(manifest['store_bytes_by_root']['docker'],
                        manifest['store_bytes_by_root']['containerd'])

    def test_repo_digest_comparison_ignores_the_tag(self):
        """The store records `postgres@sha256:...`; a tagged comparison would miss it."""
        self.guest_with_runner()
        result = self.run_prepare()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('does not record', result.stderr)

    def test_the_tag_is_what_makes_the_layers_reachable(self):
        """A job pulls by tag, so an untagged image would not help it."""
        self.guest_with_runner()
        self.seed_store(tagged=False)
        self.assertEqual(self.run_prepare().returncode, 0)
        self.assertIn('postgres:18.0', self.docker_state()['tags'])

    def test_wrong_digest_in_store_is_refused(self):
        self.guest_with_runner()
        # A registry that serves different content for the pinned reference: the pull
        # succeeds, and only the recorded digest shows what actually arrived.
        state = self.docker_state()
        state['served'] = {POSTGRES: 'postgres@sha256:' + '9' * 64}
        self.save_state(state)
        result = self.run_prepare()
        self.assertEqual(result.returncode, 1)
        self.assertIn('does not record', result.stderr)

    def test_extra_image_in_store_is_refused(self):
        self.guest_with_runner()
        result = self.run_prepare()
        self.assertEqual(result.returncode, 0)
        # Add an unrelated image, the way an earlier job or a second bake would.
        self.add_image('alpine:3.21', 'sha256:' + 'd' * 64)
        second = self.run_prepare()
        self.assertEqual(second.returncode, 1)
        self.assertIn('alpine:3.21', second.stderr)

    def test_pre_existing_auth_config_is_refused_before_any_pull(self):
        self.guest_with_runner()
        self.touch('/root/.docker/config.json', '{"auths":{}}')
        result = self.run_prepare()
        self.assertEqual(result.returncode, 1)
        self.assertIn('Refusing to add to a guest holding', result.stderr)
        self.assertEqual([line for line in self.calls('docker') if ' pull ' in line], [])

    def test_registered_runner_is_refused(self):
        self.guest_with_runner()
        self.touch('/home/runner/actions-runner/.runner')
        self.assertEqual(self.run_prepare().returncode, 1)

    def test_bootstrap_marker_is_refused(self):
        self.guest_with_runner()
        self.touch('/var/lib/runway/bootstrap.json')
        result = self.run_prepare()
        self.assertEqual(result.returncode, 1)
        self.assertIn('JIT bootstrap marker', result.stderr)

    def test_guest_with_containers_is_refused(self):
        self.guest_with_runner()
        state = self.docker_state()
        state['containers'] = ['abc123']
        self.save_state(state)
        result = self.run_prepare()
        self.assertEqual(result.returncode, 1)
        self.assertIn('already has containers or volumes', result.stderr)

    def test_flag_is_required_and_root_is_required(self):
        self.guest_with_runner()
        missing = self.run_script(PREPARE, [], name='prepare.sh')
        self.assertEqual(missing.returncode, 2)
        self.assertIn(BUILDER_FLAG, missing.stderr)
        not_root = self.run_script(PREPARE, [BUILDER_FLAG], euid=1000, name='prepare.sh')
        self.assertEqual(not_root.returncode, 1)
        self.assertIn('requires root', not_root.stderr)



class SealTests(Harness):
    def seal(self, *extra, euid=0):
        return self.run_script(SEAL, [BUILDER_FLAG, *extra], euid=euid, name='seal.sh')

    def test_default_seal_wipes_the_container_store(self):
        self.fresh_guest()
        result = self.seal()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(Path(self.root + '/var/lib/docker').exists())
        self.assertFalse(Path(self.root + '/var/lib/containerd').exists())
        self.assertIn('systemctl stop docker.service docker.socket containerd.service '
                      'atlas-metadata.service', self.calls('systemctl'))
        self.assertIn('Sealed unregistered image;', result.stdout)

    def test_preserving_keeps_the_store_and_stops_the_daemons(self):
        self.fresh_guest()
        result = self.seal(PRESERVE_FLAG)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(Path(self.root + '/var/lib/docker').is_dir())
        state = self.docker_state()
        self.assertIn('docker.service', state['stops'])
        self.assertIn('containerd.service', state['stops'])
        self.assertIn('pre-pulled service images', result.stdout)

    def test_store_is_not_stopped_before_validation(self):
        """Every guard runs while the daemon is still up, so a refusal is recoverable."""
        self.fresh_guest()
        self.touch('/home/runner/actions-runner/.runner')
        self.assertEqual(self.seal(PRESERVE_FLAG).returncode, 1)
        self.assertNotIn('docker.service', self.docker_state()['stops'])

    # -- the baked bench -----------------------------------------------------
    # A Frappe image can carry a bench with or without container images, so these
    # cases run on both seal paths.
    def claim_bench(self):
        manifest = Path(self.root + '/etc/runway-runner-image.txt')
        manifest.write_text(manifest.read_text() + 'FRAPPE_BENCH_BAKED=yes\n')

    def test_a_claimed_bench_with_no_manifest_is_refused_on_both_paths(self):
        for extra in ([], [PRESERVE_FLAG]):
            with self.subTest(preserve=bool(extra)):
                self.setUp()
                self.fresh_guest()
                self.claim_bench()
                result = self.seal(*extra)
                self.assertEqual(result.returncode, 1, result.stdout)
                self.assertIn('No bench manifest to validate', result.stderr)

    def test_a_manifest_without_a_claim_is_refused(self):
        """A half-baked guest must not be sealed as a plain profile by accident."""
        for extra in ([], [PRESERVE_FLAG]):
            with self.subTest(preserve=bool(extra)):
                self.setUp()
                self.fresh_guest()
                self.touch('/etc/runway-frappe-bench.json')
                result = self.seal(*extra)
                self.assertEqual(result.returncode, 1, result.stdout)
                self.assertIn('does not claim one', result.stderr)

    def bench_size_mib(self, bench, key):
        """The measured size of each tree the manifest records, in whole MiB.

        The validator compares each record against `du -sm` with a tolerance, so a fixture has
        to state what that measurement says rather than a guessed number; recording 1 MiB for
        an empty tree tripped the check before the test reached the guard it names.

        The same `du` the validator will use is the one this asks, by name, out of the
        harness's own bin directory. Under test PATH that is the mock, which answers with the
        builder's recorded figures; calling the system binary instead made the fixture and the
        validator disagree about the same tree, which is a failure of the fixture and not
        something the guard under test should be judged on.
        """
        relative = {'app': 'apps/frappe', 'assets': 'sites/assets', 'env': 'env',
                    'node_modules': 'apps/frappe/node_modules'}[key]
        du = shutil.which('du', path=self.bin)
        return int(subprocess.run([du, '-sm', str(bench / relative)], capture_output=True,
                                  text=True, check=True).stdout.split()[0])

    def write_valid_bench_manifest(self, **changes):
        """Write a manifest that satisfies every check, then apply `changes` to it.

        The archive, the bench directory and its git checkout all have to exist, because
        the validator checks them in that order and stops at the first failure. A fixture
        that omits the archive stops at the key set, and a test asserting on a later check
        then passes for the wrong reason: it never exercised the guard it names.
        """
        bench = Path(self.root + '/home/runner/work/frappe/frappe')
        for directory in ('apps/frappe', 'sites/assets', 'config', 'logs', 'config/pids'):
            (bench / directory).mkdir(parents=True, exist_ok=True)
        (bench / 'env/bin').mkdir(parents=True, exist_ok=True)
        (bench / 'env/bin/python').write_text('#!/bin/sh\n')
        (bench / 'apps/frappe/frappe').mkdir(parents=True, exist_ok=True)
        (bench / 'apps/frappe/frappe/__init__.py').write_text('')
        (bench / 'apps/frappe/node_modules').mkdir(parents=True, exist_ok=True)
        (bench / 'sites/assets/frappe').mkdir(parents=True, exist_ok=True)
        (bench / 'sites/common_site_config.json').write_text('{}')
        (bench / 'Procfile').write_text('web: bench serve\n')
        app = bench / 'apps/frappe'
        if not (app / '.git').exists():
            subprocess.run(['git', 'init', '-q', str(app)], check=True)
            subprocess.run(['git', '-C', str(app), 'add', '-A'], check=True,
                           capture_output=True)
            subprocess.run(
                ['git', '-C', str(app), '-c', 'user.email=t@e', '-c', 'user.name=t',
                 'commit', '-q', '-m', 'baked'],
                check=True, capture_output=True)
        head = subprocess.run(['git', '-C', str(app), 'rev-parse', 'HEAD'],
                              capture_output=True, text=True, check=True).stdout.strip()
        # The archive path is recorded as the absolute test path rather than
        # /opt/runway: the validator opens whatever the manifest names, on the real
        # filesystem, and /opt/runway is not a harness-owned path. A guest that wrote
        # here during a test would be writing to the machine running the tests.
        archive = Path(self.root + '/opt/runway/bench.tar.gz')
        archive.parent.mkdir(parents=True, exist_ok=True)
        archive.write_bytes(b'bench')
        manifest = {
            'archive': {'path': str(archive),
                        'sha256': hashlib.sha256(b'bench').hexdigest(),
                        'bytes': len(b'bench')},
            'bench_root': str(bench),
            'frappe_commit': head, 'frappe_ref': head,
            'frappe_repository': 'https://github.com/example/frappe.git',
            'node_version': '24.21.0', 'python_version': '3.14.7',
            'size_mib': {key: self.bench_size_mib(bench, key) for key in
                         ('app', 'assets', 'env', 'node_modules')},
        }
        manifest.update(changes)
        self.touch('/etc/runway-frappe-bench.json', json.dumps(manifest))
        return manifest

    def test_a_valid_bench_manifest_is_accepted(self):
        """The fixture the other tests spoil has to pass on its own."""
        self.fresh_guest()
        self.claim_bench()
        self.write_valid_bench_manifest()
        self.assertEqual(self.seal().returncode, 0)

    def test_a_manifest_whose_bench_root_is_not_there_is_refused(self):
        for extra in ([], [PRESERVE_FLAG]):
            with self.subTest(preserve=bool(extra)):
                self.setUp()
                self.fresh_guest()
                self.claim_bench()
                self.write_valid_bench_manifest(
                    bench_root=str(self.root + '/home/runner/work/frappe/absent'))
                result = self.seal(*extra)
                self.assertEqual(result.returncode, 1, result.stdout)
                self.assertIn('not a directory', result.stderr,
                              f'the bench_root check never ran; stderr was {result.stderr}')

    def test_a_manifest_whose_commit_is_not_the_baked_one_is_refused(self):
        self.fresh_guest()
        self.claim_bench()
        self.write_valid_bench_manifest(frappe_commit='4' * 40)
        result = self.seal()
        self.assertEqual(result.returncode, 1, result.stdout)
        self.assertIn('claims the app at', result.stderr)

    def test_preserve_requires_the_manifest(self):
        self.fresh_guest()
        Path(self.root + '/etc/runway-container-images.json').unlink()
        result = self.seal(PRESERVE_FLAG)
        self.assertEqual(result.returncode, 1)
        self.assertIn('runway-container-images.json', result.stderr)

    def test_manifest_with_a_fourth_image_is_refused(self):
        self.fresh_guest()
        document = json.loads(Path(self.root + '/etc/runway-container-images.json').read_text())
        extra = dict(document['images'][0])
        extra['reference'] = 'alpine:3.21@sha256:' + '1' * 64
        extra['repository'], extra['tag'] = 'alpine', '3.21'
        document['images'].append(extra)
        document['image_count'] = 4
        Path(self.root + '/etc/runway-container-images.json').write_text(json.dumps(document))
        result = self.seal(PRESERVE_FLAG)
        self.assertEqual(result.returncode, 1)
        self.assertIn('this seal preserves 3', result.stderr)

    def test_manifest_with_an_unexpected_key_is_refused(self):
        self.fresh_guest()
        document = json.loads(Path(self.root + '/etc/runway-container-images.json').read_text())
        document['registry_auth'] = 'Bearer hunter2'
        Path(self.root + '/etc/runway-container-images.json').write_text(json.dumps(document))
        result = self.seal(PRESERVE_FLAG)
        self.assertEqual(result.returncode, 1)
        self.assertIn('unexpected', result.stderr)

    def test_manifest_naming_the_wrong_digest_is_refused(self):
        self.fresh_guest()
        document = json.loads(Path(self.root + '/etc/runway-container-images.json').read_text())
        document['images'][0]['reference'] = 'postgres:17.0@sha256:' + '2' * 64
        Path(self.root + '/etc/runway-container-images.json').write_text(json.dumps(document))
        result = self.seal(PRESERVE_FLAG)
        self.assertEqual(result.returncode, 1)
        self.assertIn('this seal only preserves', result.stderr)

    def test_manifest_agreeing_with_nothing_in_the_store_is_refused(self):
        self.fresh_guest()
        document = json.loads(Path(self.root + '/etc/runway-container-images.json').read_text())
        for entry in document['images']:
            entry['amd64_image_id'] = 'sha256:' + '7' * 64
        Path(self.root + '/etc/runway-container-images.json').write_text(json.dumps(document))
        result = self.seal(PRESERVE_FLAG)
        self.assertEqual(result.returncode, 1)
        self.assertIn('different image content', result.stderr)

    def test_store_holding_an_extra_image_is_refused(self):
        self.fresh_guest()
        result = self.seal(PRESERVE_FLAG)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.add_image('alpine:3.21', 'sha256:' + 'd' * 64)
        second = self.seal(PRESERVE_FLAG)
        self.assertEqual(second.returncode, 1)
        self.assertIn('outside the pinned set', second.stderr)

    def test_store_missing_a_pinned_image_is_refused(self):
        self.fresh_guest()
        state = self.docker_state()
        state['tags'] = [tag for tag in state['tags'] if tag != MARIADB.split('@')[0]]
        state['repo_digests'].pop(MARIADB, None)
        state['image_ids'].pop(MARIADB, None)
        self.save_state(state)
        result = self.seal(PRESERVE_FLAG)
        self.assertEqual(result.returncode, 1)
        self.assertIn('mariadb@sha256:', result.stderr)

    def test_dangling_image_is_refused(self):
        self.fresh_guest()
        state = self.docker_state()
        state['dangling'] = ['sha256:' + '8' * 64]
        self.save_state(state)
        result = self.seal(PRESERVE_FLAG)
        self.assertEqual(result.returncode, 1)
        self.assertIn('dangling images are present', result.stderr)

    def test_containers_refuse_the_preserve_path(self):
        self.fresh_guest()
        state = self.docker_state()
        state['containers'] = ['abc123']
        self.save_state(state)
        result = self.seal(PRESERVE_FLAG)
        self.assertEqual(result.returncode, 1)
        self.assertIn('this guest has containers', result.stderr)

    def test_bootstrap_marker_refuses_the_preserve_path(self):
        self.fresh_guest()
        self.touch('/var/lib/runway/bootstrap.json')
        result = self.seal(PRESERVE_FLAG)
        self.assertEqual(result.returncode, 1)
        self.assertIn('a job ran in this guest', result.stderr)

    def test_auth_config_refuses_the_preserve_path_before_the_daemon_stops(self):
        self.fresh_guest()
        self.touch('/home/runner/.docker/config.json', '{"auths":{}}')
        result = self.seal(PRESERVE_FLAG)
        self.assertEqual(result.returncode, 1)
        self.assertIn('authenticated a registry', result.stderr)
        self.assertNotIn('docker.service', self.docker_state()['stops'])

    def test_ssh_host_keys_and_history_are_scrubbed_on_both_paths(self):
        for arguments in ([], [PRESERVE_FLAG]):
            with self.subTest(preserve=bool(arguments)):
                self.setUp()
                self.fresh_guest()
                self.touch('/root/.bash_history')
                self.assertEqual(self.seal(*arguments).returncode, 0)
                self.assertFalse(Path(self.root + '/etc/ssh/ssh_host_ed25519_key').exists())
                self.assertFalse(Path(self.root + '/root/.bash_history').exists())

    def test_sshd_is_stopped_before_the_host_keys_are_deleted(self):
        """sshd regenerates a missing host key, so the order is the whole safeguard.

        A guest sealed with sshd still running deleted the keys, then grew a fresh
        identity four minutes later. The export carried a host key and the finalizer
        refused it. Stopping the service first is what makes the deletion stick.
        """
        self.fresh_guest()
        self.assertEqual(self.seal().returncode, 0)
        stops = [call for call in self.calls('systemctl') if call.split(' ', 1)[1].startswith('stop')]
        ssh_stop = next((call for call in stops if 'ssh' in call), None)
        self.assertIsNotNone(ssh_stop, f'no ssh service was stopped; stops were {stops}')
        # The stop has to precede the deletion, and only the unfiltered log records an
        # order across two different tools: calls('systemctl') and calls('find') are each
        # filtered to one tool, so an index into one and a length from the other compare
        # two unrelated quantities. Positions in calls() are the file's own order.
        everything = self.calls()
        removes = [(index, call) for index, call in enumerate(everything) if 'ssh_host_' in call]
        self.assertTrue(removes, f'no ssh_host_ removal was recorded; log was {everything}')
        self.assertLess(everything.index(ssh_stop), removes[0][0],
                        'the key removal has to follow the ssh stop')

    def test_builder_flag_is_required(self):
        self.fresh_guest()
        result = self.run_script(SEAL, [], name='seal.sh')
        self.assertEqual(result.returncode, 2)
        self.assertIn(BUILDER_FLAG, result.stderr)
        self.assertTrue(Path(self.root + '/var/lib/docker').is_dir(),
                        'a missing flag must not have sealed anything')

    def test_non_root_is_refused(self):
        self.fresh_guest()
        self.assertEqual(self.seal(euid=1000).returncode, 2)

    def test_registered_runner_is_refused_on_both_paths(self):
        for arguments in ([], [PRESERVE_FLAG]):
            with self.subTest(preserve=bool(arguments)):
                self.setUp()
                self.fresh_guest()
                self.touch('/home/runner/actions-runner/.credentials')
                result = self.seal(*arguments)
                self.assertEqual(result.returncode, 1)
                self.assertIn('registered runner', result.stderr)


class FinalizeSizeTests(Harness):
    """`--size-gib` is validated before any tool touches the export."""

    def setUp(self):
        super().setUp()
        self.export = Path(self.tmp / 'runner.ext4')
        self.export.write_bytes(b'\0' * 4096)
        # A builder exports a filesystem larger than the published target: shrinking
        # is the normal case, and the finalizer has to resize rather than refuse.
        self.install_fs_tools(blocks=10 * 1024 * 1024, block_size=1024)

    def test_default_is_four_gib_and_out_of_range_values_are_usage_errors(self):
        for value in ('3', '17', '64', '0', '-1', 'seven', '4.5', '04', ''):
            with self.subTest(value=value):
                result = self.run_script(FINALIZE, [f'--size-gib={value}', str(self.export)],
                                         name='finalize.sh')
                self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
                # A rejected size must fail before the minutes-long filesystem work.
                self.assertEqual(self.calls('systemctl'), [])
                self.assertNotIn('e2fsck', result.stdout)

    def test_bounds_accept_the_range_endpoints(self):
        for value in ('4', '7', '16'):
            with self.subTest(value=value):
                self.setUp()
                self.install_fs_tools(blocks=10 * 1024 * 1024, block_size=1024)
                self.export.write_bytes(b'\0' * 4096)
                result = self.run_script(FINALIZE, [f'--size-gib={value}', str(self.export)],
                                         name='finalize.sh')
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn(f'resize2fs {self.export} {value}G', self.calls('resize2fs'))

    def test_a_larger_export_is_resized_not_refused(self):
        """A 10 GiB builder publishing at 7 GiB is a shrink, and must proceed."""
        self.install_fs_tools(blocks=10 * 1024 * 1024, block_size=1024)
        result = self.run_script(FINALIZE, ['--size-gib', '7', str(self.export)], name='finalize.sh')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotIn('does not fit', result.stderr)
        self.assertNotIn('Truncating would discard', result.stderr)

    def test_resize_happens_before_truncate(self):
        self.install_fs_tools(blocks=10 * 1024 * 1024, block_size=1024)
        order = [line.split(' ', 1)[0] for line in self.calls()
                 if line.split(' ', 1)[0] in ('resize2fs', 'truncate')]
        self.assertEqual(self.run_script(FINALIZE, ['--size-gib', '7', str(self.export)],
                                         name='finalize.sh').returncode, 0)
        order = [line.split(' ', 1)[0] for line in self.calls()
                 if line.split(' ', 1)[0] in ('resize2fs', 'truncate')]
        self.assertEqual(order[0], 'resize2fs', 'the filesystem is resized before the file is cut')
        self.assertIn('truncate', order)
        # And the truncate targets exactly the published size.
        self.assertIn(f'truncate -s {7 * 1024 ** 3} {self.export}', self.calls('truncate'))

    def test_a_resize_that_did_not_take_is_refused_before_truncating(self):
        # resize2fs succeeds but the filesystem still reports the larger capacity, which
        # is what a silent no-op resize looks like. Cutting the file to the target then
        # would discard content, so the export must stop instead.
        self.install_fs_tools(blocks=10 * 1024 * 1024, block_size=1024,
                              resized_blocks=10 * 1024 * 1024)
        result = self.run_script(FINALIZE, ['--size-gib', '7', str(self.export)], name='finalize.sh')
        self.assertEqual(result.returncode, 1)
        self.assertIn('would discard content', result.stderr)
        self.assertEqual([line for line in self.calls('truncate')], [])

    def test_credential_guards_still_run_before_the_resize(self):
        self.install_fs_tools(blocks=10 * 1024 * 1024, block_size=1024)
        result = self.run_script(FINALIZE, ['--size-gib', '7', str(self.export)], name='finalize.sh')
        self.assertEqual(result.returncode, 0)
        stat_calls = [line for line in self.calls('debugfs') if line.startswith('debugfs stat ')]
        # Each guard is matched by the guest path it ends with, because the stat calls are
        # routed into the private tree.
        for suffix in ('/etc/runway-runner-image.txt',
                       '/home/runner/actions-runner/.runner',
                       '/home/runner/actions-runner/.credentials',
                       '/home/runner/actions-runner/.credentials_rsaparams',
                       '/etc/ssh/ssh_host_ed25519_key', '/etc/ssh/ssh_host_rsa_key'):
            self.assertTrue(any(line.endswith(suffix) for line in stat_calls),
                            f'{suffix} was never checked')
        # Every credential guard runs before the filesystem is resized, so a refusal never
        # leaves a half-published export. The stat calls are the guards; the later stats
        # calls read capacity, and come after the resize by design.
        order = [line.split(' ', 1)[0] for line in self.calls()]
        guards = [index for index, line in enumerate(self.calls())
                  if line.startswith('debugfs stat ')]
        self.assertTrue(guards)
        self.assertLess(max(guards), order.index('resize2fs'))

    def test_exactly_one_exported_filesystem_is_required(self):
        second = Path(self.tmp / 'other.ext4')
        second.write_bytes(b'\0' * 4096)
        result = self.run_script(FINALIZE, [str(self.export), str(second)], name='finalize.sh')
        self.assertEqual(result.returncode, 2)
        self.assertIn('exactly one exported filesystem', result.stderr)

    def test_missing_or_symlinked_export_is_refused(self):
        missing = self.run_script(FINALIZE, [str(self.tmp / 'absent.ext4')], name='finalize.sh')
        self.assertEqual(missing.returncode, 2)
        link = Path(self.tmp / 'link.ext4')
        link.symlink_to(self.export)
        self.assertEqual(self.run_script(FINALIZE, [str(link)], name='finalize.sh').returncode, 2)

    def test_flag_may_follow_the_image_path(self):
        result = self.run_script(FINALIZE, [str(self.export), '--size-gib', '7'], name='finalize.sh')
        self.assertNotIn('--size-gib must be', result.stderr)
        self.assertNotIn('exactly one exported filesystem', result.stderr)


class BakeRecipeTests(Harness):
    """The bake's refusals, and the one thing it asserts about its own output.

    The bake is 300-odd lines of network, package and bench work that cannot run here, but
    every guard in front of it is pure and runs against a stub. That matters most for the
    archive check: the check reads the tar's own listing, so a test asserts against a
    listing the harness produces rather than against the directory that produced it.
    """

    def setUp(self):
        super().setUp()
        Path(self.root + '/etc').mkdir(parents=True, exist_ok=True)
        Path(self.root + '/etc/os-release').write_text('ID=ubuntu\nVERSION_ID="24.04"\n')
        self.bench = Path(self.root + '/home/runner/work/frappe/frappe')
        self.runner = Path(self.root + '/home/runner/actions-runner')
        (self.runner / 'bin').mkdir(parents=True)
        (self.runner / 'bin' / 'Runner.Listener').write_text('')
        (self.runner / 'bin' / 'Runner.Listener').chmod(0o755)
        Path(self.root + '/etc/runway-runner-image.txt').write_text('OS=Ubuntu 24.04\n')
        # A real /tmp in the private tree, in the state the base image should ship. The bake
        # stats both the mode and the owner before several of its refusals, so a missing or
        # wrongly-owned /tmp stops the run before the guard under test is reached. The mode is
        # set directly; the owner cannot be, because these tests run as whatever user invoked
        # them and a test must not require root to chown. stat is stubbed to report the owner
        # instead, which is also the only way to exercise a wrong one.
        private_tmp = Path(self.root) / 'tmp'
        private_tmp.mkdir(parents=True, exist_ok=True)
        private_tmp.chmod(0o1777)
        self.mock_shell('stat', '''
case "$2" in
  %a|%a/*) printf '1777\\n' ;;
  %U|%U/*) printf 'root\\n' ;;
  *) exec /usr/bin/stat "$@" ;;
esac
''')

    def tool_cache(self):
        """The three toolchain entries the bake refuses to start without.

        These are checked before anything the tests here assert on, so a fixture that
        omits them reports a missing interpreter instead of the guard under test. They are
        executable stubs rather than the real toolchain: nothing in these tests runs the
        interpreter, and the bake gets as far as these only to learn they are present.
        """
        python = Path(self.root + '/opt/hostedtoolcache/Python/3.14.7/x64/bin/python3')
        node = Path(self.root + '/opt/hostedtoolcache/node/24.21.0/x64/bin/node')
        for stub in (python, node):
            stub.parent.mkdir(parents=True, exist_ok=True)
            stub.write_text('#!/bin/sh\nexit 0\n')
            stub.chmod(0o755)
        for tool in ('/usr/local/bin/yarn', '/usr/local/bin/uv'):
            stub = Path(self.root + tool)
            stub.parent.mkdir(parents=True, exist_ok=True)
            stub.write_text('#!/bin/sh\nexit 0\n')
            stub.chmod(0o755)

    def run_bake(self, *extra, euid=0):
        # The bake resolves its archive checker at ../scripts/, so the routed copy lives
        # in guest/ and the checker is staged in scripts/ beside it, matching the layout
        # the builder carries.
        scripts = Path(self.tmp / 'scripts')
        scripts.mkdir(parents=True, exist_ok=True)
        (scripts / 'check_bench_archive.sh').write_text(
            (HERE / 'check_bench_archive.sh').read_text())
        return self.run_script(BAKE, [BUILDER_FLAG, *extra], euid=euid, name='bake.sh',
                               in_guest_dir=True)

    def full_bench(self):
        """A bench tree with everything the bake's own checks look for."""
        for directory in ('apps/frappe', 'sites/assets', 'config', 'logs', 'config/pids'):
            (self.bench / directory).mkdir(parents=True, exist_ok=True)
        (self.bench / 'env/bin').mkdir(parents=True, exist_ok=True)
        (self.bench / 'env/bin/python').write_text('#!/bin/sh\n')
        (self.bench / 'apps/frappe/frappe').mkdir(parents=True, exist_ok=True)
        (self.bench / 'apps/frappe/frappe/__init__.py').write_text('')
        (self.bench / 'apps/frappe/node_modules').mkdir(parents=True, exist_ok=True)
        (self.bench / 'sites/assets/frappe').mkdir(parents=True, exist_ok=True)
        (self.bench / 'sites/common_site_config.json').write_text('{}')
        (self.bench / 'Procfile').write_text('web: bench serve\n')
        (self.bench / 'logs/dev.log').write_text('noise\n')

    # The refusals. Each is a case where carrying on would publish something unusable.

    def test_the_bake_leaves_the_work_folder_owned_by_the_runner(self):
        """The runner creates _tool inside the work folder, so it must own the folder.

        The runner runs as the runner account and creates _tool directly under the work
        folder it was registered with. A root-owned /home/runner/work fails the job at
        startup with UnauthorizedAccessException before any step runs, which is what a
        build that creates only the bench directories produces: the bench was correct and
        runner-owned, and the job still could not start.
        """
        text = BAKE.read_text()
        self.assertIn('install -d -m 0755 -o runner -g runner "$runner_workspace"', text)
        # It has to be created with a trailing slash-free path, or it names the bench
        # directory instead and the ownership is applied where it already was.
        self.assertNotIn('-o runner -g runner "$runner_workspace/"', text)

    def test_the_bake_needs_a_tmp_the_apt_account_can_write(self):
        """The bake refuses rather than repairing /tmp, and says why.

        The profile step repairs it first, because every builder from this base image
        needs it and the apt message names nothing real. The bake's own guard is kept as a
        check rather than a fix: a builder whose /tmp is wrong at that point has been built
        wrongly, and silently correcting it would hide that.
        """
        # Both steps repair it, because the bake can run alone against a throwaway guest
        # and nothing else will have. It has to be reported, so a guest that arrived wrong
        # is visible rather than silently corrected.
        guard = BAKE.read_text().split('if [[ $(stat -c %a /tmp)')[1].split('\nfi')[0]
        self.assertIn('chmod 1777 /tmp', guard)
        self.assertIn('Repairing /tmp', guard)
        profile = (GUEST / 'prepare-frappe-image.sh').read_text()
        self.assertIn('chmod 1777 /tmp', profile)

    def test_the_profile_repairs_tmp_before_the_first_apt(self):
        """The repair is in the profile, before apt, because the base image is wrong.

        Two builds failed on four "repository is not signed" errors that name only apt. The
        fix belongs where every builder passes, which is the profile step, and before its
        first apt call rather than after a failure.
        """
        text = (GUEST / 'prepare-frappe-image.sh').read_text()
        repair = text.index('chmod 1777 /tmp')
        first_apt = text.index('apt-get update')
        self.assertLess(repair, first_apt,
                        'the repair has to happen before apt can fail on it')

    def test_the_profile_does_not_build_under_an_upgrade(self):
        """unattended-upgrades holds the dpkg lock on every boot, so the first apt fails.

        It also mutates the image while this recipe builds it, which would make two builds
        of one base image differ. It is stopped and masked before the first apt, and the
        wait for the running process is there because stopping the unit does not stop a
        process it already started.
        """
        text = (GUEST / 'prepare-frappe-image.sh').read_text()
        stop = text.index('systemctl stop unattended-upgrades.service')
        first_apt = text.index('apt-get update')
        self.assertLess(stop, first_apt, 'the dpkg lock has to be released before apt')
        self.assertIn('systemctl mask unattended-upgrades.service', text)
        self.assertLess(text.index('while pgrep -x unattended-upgr'), first_apt,
                        'stopping the unit does not stop the process it already started')

    def test_the_flag_is_required(self):
        result = self.run_script(BAKE, [], name='bake.sh', in_guest_dir=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn('Usage inside an unregistered builder guest', result.stderr)

    def test_root_is_required(self):
        result = self.run_bake(euid=1001)
        self.assertEqual(result.returncode, 1)
        self.assertIn('requires root', result.stderr)

    def test_an_image_without_the_runner_profile_is_refused(self):
        # The profile manifest and the runner binary are removed, because setUp provides
        # them; leaving them in place made this test fail on the later tool-cache guard
        # and assert about a check it never reached.
        Path(self.root + '/etc/runway-runner-image.txt').unlink()
        (self.runner / 'bin' / 'Runner.Listener').unlink()
        result = self.run_bake()
        self.assertEqual(result.returncode, 1)
        self.assertIn('extends that image', result.stderr)

    def test_a_registered_runner_is_refused(self):
        self.tool_cache()
        self.touch('/home/runner/actions-runner/.runner', '{}\n')
        result = self.run_bake()
        self.assertEqual(result.returncode, 1)
        self.assertIn('Refusing to bake into a registered runner', result.stderr)

    def test_a_jit_bootstrap_marker_is_refused(self):
        self.tool_cache()
        self.touch('/var/lib/runway/bootstrap.json', '{}\n')
        result = self.run_bake()
        self.assertEqual(result.returncode, 1)
        self.assertIn('JIT bootstrap marker', result.stderr)

    def test_a_second_bake_is_refused(self):
        self.tool_cache()
        self.touch('/etc/runway-frappe-bench.json', '{}\n')
        result = self.run_bake()
        self.assertEqual(result.returncode, 1)
        self.assertIn('already carries a baked bench', result.stderr)

    def test_a_bench_root_with_no_manifest_is_refused(self):
        # bench init finds apps it cannot account for and offers to roll back, which is a
        # question a build script cannot answer.
        self.tool_cache()
        self.full_bench()
        result = self.run_bake()
        self.assertEqual(result.returncode, 1)
        self.assertIn('no manifest to explain it', result.stderr)

    def test_a_tmp_the_apt_account_cannot_write_is_repaired_and_reported(self):
        """A /tmp that is not root:1777 makes every apt repository report "not signed".

        The builder this branch used had /tmp owned by ubuntu at 775, so apt dropped to
        _apt to verify a signature, could not write its temporary configuration, and
        reported four unsigned repositories. Nothing in that message points at /tmp, and
        the cost was two builds failing on an apt error naming no real fault.

        It is repaired rather than refused, because the bake can run on its own against a
        throwaway guest where the profile step never ran, and a refusal there would mean
        the bake could only ever run on an image the profile had already fixed. Reported as
        well as repaired, so a guest that arrived wrong is visible.
        """
        self.tool_cache()
        # The stub answers only for /tmp and defers to the real stat otherwise, so a
        # later stat of a real file still measures it. Answering for every path would
        # make this test pass for reasons it does not mean.
        # The stub answers for the mode and the owner and defers to the real stat otherwise.
        # The private root has a real /tmp from setUp, so nothing here depends on a path
        # being absent.
        # Replaces the setUp stub: a /tmp the apt account cannot write. stat is called as
        # `stat -c FORMAT path`, so the format is $2.
        self.mock_shell('stat', '''
case "$2" in
  %a|%a/*) printf '775\\n' ;;
  %U|%U/*) printf 'ubuntu\\n' ;;
  %U:%a|%U:%a/*) printf 'ubuntu:775\\n' ;;
  *) exec /usr/bin/stat "$@" ;;
esac
''')
        result = self.run_bake()
        # The repair happens; the run then stops for its own reasons, which this test does
        # not claim to cover. What it claims is that a wrong /tmp is reported rather than
        # silently corrected, naming both what it found and what it wanted.
        self.assertIn('Repairing /tmp', result.stderr)
        self.assertIn('ubuntu:775 rather than root:1777', result.stderr)

    def test_a_tmpfs_over_the_workspace_is_refused(self):
        # The workspace tmpfs opt-in mounts over this directory and would hide the bake.
        self.tool_cache()
        self.mock_shell('mountpoint', 'exit 0\n')
        result = self.run_bake()
        self.assertEqual(result.returncode, 1)
        self.assertIn('would be hidden by it', result.stderr)

    # The bake's own output check, which is the guard added for the bug this branch fixed.

    def test_the_bake_checks_the_archive_with_a_script(self):
        """The archive check is a separate script, so it can be called on its own.

        Driving it through the bake would mean running a 300-line network and package
        build to reach one grep, and a test that needs a real bench to check an archive is
        a test that stops being run. The bake's refusal to run without it is asserted
        separately, because that is the half that can regress silently.
        """
        text = BAKE.read_text()
        self.assertIn('check_bench_archive.sh', text)
        self.assertTrue((HERE / 'check_bench_archive.sh').is_file())


class ArchiveCheckTests(Harness):
    """The archive check, called directly on archives the test builds.

    The bake shells out to this, so it is a script rather than inline logic, and these
    tests use the real `tar` rather than a stub: the check's whole subject is what tar
    wrote, so asserting against a hand-written listing would test the stub.
    """

    def archive_of(self, exclude=()):
        """A real gzip tar of a bench tree, optionally with paths left out."""
        tree = Path(self.tmp / 'tree')
        for directory in ('apps/frappe', 'sites/assets/frappe', 'config', 'logs',
                          'config/pids'):
            (tree / directory).mkdir(parents=True, exist_ok=True)
        (tree / 'Procfile').write_text('web: bench serve\n')
        (tree / 'logs/dev.log').write_text('noise\n')
        packed = Path(self.tmp / 'bench.tar')
        # The names become --exclude flags here rather than at each call site: a bare
        # `apps` is a file operand to tar, not a pattern, and tar then reports a missing
        # file rather than quietly packing everything.
        flags = [f'--exclude={name}' for name in exclude]
        subprocess.run(['tar', '-C', str(tree), *flags, '-cf', '-', '.'],
                       stdout=packed.open('wb'), check=True)
        subprocess.run(['gzip', '-f', str(packed)], check=True)
        # gzip -f on bench.tar leaves bench.tar.gz, so the name is not a suffix swap.
        return Path(str(packed) + '.gz')

    def check(self, archive):
        return subprocess.run(['bash', str(HERE / 'check_bench_archive.sh'), str(archive)],
                              capture_output=True, text=True)

    def test_a_complete_archive_is_accepted(self):
        result = self.check(self.archive_of())
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_each_directory_bench_requires_is_checked(self):
        """One case per directory, because the guard is a list and a list rots quietly."""
        for required in ('apps', 'sites', 'config', 'logs', 'config/pids'):
            with self.subTest(missing=required):
                self.setUp()
                archive = self.archive_of(exclude=(required,))
                result = self.check(archive)
                self.assertEqual(result.returncode, 1, f'{required} was not checked')
                self.assertIn(f'no {required}, which bench requires', result.stderr)

    def test_a_missing_archive_is_refused(self):
        result = self.check(Path(self.tmp / 'absent.tar.gz'))
        self.assertEqual(result.returncode, 1)
        self.assertIn('No archive to check', result.stderr)

    def test_no_argument_is_a_usage_error(self):
        result = subprocess.run(['bash', str(HERE / 'check_bench_archive.sh')],
                                capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn('Usage:', result.stderr)


if __name__ == '__main__':
    unittest.main()