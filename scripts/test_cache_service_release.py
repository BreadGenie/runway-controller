"""Offline archive and root-prefixed installer contract tests."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest

REPO = Path(__file__).resolve().parents[1]

class ReleaseTest(unittest.TestCase):
    def test_release(self):
        with tempfile.TemporaryDirectory() as temp:
            temp = Path(temp)
            archives = [temp / 'one.tar.gz', temp / 'two.tar.gz']
            for archive in archives:
                subprocess.run([str(REPO / 'scripts/build-cache-service-release.sh'), str(archive)], check=True)
            self.assertEqual(archives[0].read_bytes(), archives[1].read_bytes())
            with tarfile.open(archives[0]) as archive:
                archive.extractall(temp / 'package', filter='data')
            package = temp / 'package/runway-cache'
            manifest = (package / 'SHA256SUMS').read_text().splitlines()
            self.assertEqual({line.split('  ')[1] for line in manifest},
                             {str(p.relative_to(package)) for p in package.rglob('*') if p.is_file() and p.name != 'SHA256SUMS'})
            for line in manifest:
                digest, name = line.split('  ')
                self.assertEqual(digest, hashlib.sha256((package / name).read_bytes()).hexdigest())
            root = temp / 'root'
            (root / 'etc/runway').mkdir(parents=True)
            fake = temp / 'fake'
            fake.mkdir()
            log = temp / 'systemctl.log'
            (fake / 'systemctl').write_text('#!/bin/bash\nprintf "%s\\n" "$*" >> "$SYSTEMCTL_LOG"\n')
            (fake / 'systemctl').chmod(0o755)
            env = dict(os.environ, RUNWAY_ROOT=str(root), RUNWAY_DOMAIN='runway-cache-svc.example.test', PATH=str(fake)+':'+os.environ['PATH'], SYSTEMCTL_LOG=str(log))
            setup = [str(package / 'setup.sh')]
            def run(ok=True):
                result = subprocess.run(setup, env=env, capture_output=True)
                self.assertEqual(result.returncode == 0, ok, result.stderr)
            run(False)
            config = root / 'etc/runway/cache-service.json'
            config.write_text('{')
            run(False)
            data = dict(version=1, region_id='blr-1', mesh_region_id=1, central_region_id=1,
                        listen='[fdaa:1::10]:8443', operator_listen='[fdaa:1::10]:8445', central_issuer='central',
                        central_jwks_url='https://central.example/api/method/central.api.jwks.get_jwks', central_url='https://central.example', central_ca_file='',
                        central_token_file='/etc/runway/central-token', audience='https://127.0.0.1:18445', certificate_file='/etc/runway/tls/certificate.pem',
                        private_key_file='/etc/runway/tls/private-key.pem', signing_key_file='/etc/runway/signing-key.pem', cache_data_ca_file='/etc/runway/tls/ca.pem', archive_bytes=1 << 30)
            config.write_text(json.dumps(data))
            run()
            # Initialization is a service startup action, never called by setup.
            self.assertFalse((root / 'etc/runway/signing-key.pem').exists())
            if os.geteuid() == 0:
                init = [str(package / 'bin/runway'), '--action', 'cache-service-init', '--config', str(config)]
                subprocess.run(init, env=env, check=True)
                signing = (root / 'etc/runway/signing-key.pem').read_bytes()
                derived = json.loads((root / 'etc/runway/cache-data.json').read_text())
                self.assertEqual(derived['public_origin'], data['audience'])
                self.assertEqual(len(derived['public_keys']), 1)
                subprocess.run(init, env=env, check=True)
                self.assertEqual(signing, (root / 'etc/runway/signing-key.pem').read_bytes())
            first = log.read_text()
            run()
            self.assertEqual(first, log.read_text())
            binary = root / 'usr/local/bin/runway'
            binary.write_bytes(b'old binary')
            run()
            self.assertEqual(binary.with_suffix('.previous').read_bytes(), b'old binary')
            self.assertEqual((root / 'etc/runway').stat().st_mode & 0o777, 0o700)
            data['audience'] = 'runway-cache'
            config.write_text(json.dumps(data))
            run(False)
