"""Opt-in disposable real Garage S3 smoke test; no host credentials or network fetch."""
import hashlib
import os
from pathlib import Path
import secrets
import socket
import subprocess
import tempfile
import time
import unittest

GARAGE_BIN = str(Path(__file__).resolve().parents[2] / '.scratch/runway-regional-cache/vendor/garage-v2.3.0/garage')
S3_REGION = 'garage'
EXPECTED_SHA256 = 'f98d317942bb341151a2775162016bb50cf86b865d0108de03eb5db16e2120cd'

def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()

def free_port():
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port

def write_config(path, ports, rpc_secret, admin_token, metrics_token):
    cfg = (
        'metadata_dir = "%s"\n'
        'data_dir = "%s"\n'
        'db_engine = "sqlite"\n\n'
        'replication_factor = 1\n'
        'consistency_mode = "consistent"\n\n'
        'rpc_bind_addr = "127.0.0.1:%d"\n'
        'rpc_public_addr = "127.0.0.1:%d"\n'
        'rpc_secret = "%s"\n\n'
        '[s3_api]\n'
        's3_region = "%s"\n'
        'api_bind_addr = "127.0.0.1:%d"\n\n'
        '[admin]\n'
        'api_bind_addr = "127.0.0.1:%d"\n'
        'admin_token = "%s"\n'
        'metrics_token = "%s"\n'
    ) % (
        os.path.join(os.path.dirname(path), "meta"),
        os.path.join(os.path.dirname(path), "data"),
        ports["rpc"], ports["rpc"], rpc_secret,
        S3_REGION, ports["s3"], ports["admin"],
        admin_token, metrics_token,
    )
    with open(path, "w") as f:
        os.chmod(path, 0o600)
        f.write(cfg)

def start_garage(workdir, ports, access_key, secret_key, bucket):
    cfg_path = os.path.join(workdir, "garage.toml")
    write_config(
        cfg_path, ports,
        rpc_secret=secrets.token_hex(32),
        admin_token=secrets.token_urlsafe(32),
        metrics_token=secrets.token_urlsafe(32),
    )
    log_path = os.path.join(workdir, "server.log")
    log_f = open(log_path, "wb")
    env = {
        "GARAGE_DEFAULT_ACCESS_KEY": access_key,
        "GARAGE_DEFAULT_SECRET_KEY": secret_key,
        "GARAGE_DEFAULT_BUCKET": bucket,
        "RUST_LOG": "error",
    }
    proc = subprocess.Popen(
        [GARAGE_BIN, "-c", cfg_path, "server",
         "--single-node", "--default-bucket"],
        env=env, stdout=log_f, stderr=subprocess.STDOUT,
    )
    return proc, log_f, cfg_path

def stop_garage(proc, log_f):
    try:
        if proc.poll() is None:
            proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait(timeout=10)
    finally:
        try:
            log_f.close()
        except Exception:
            pass

@unittest.skipUnless(os.environ.get('RUNWAY_GARAGE_TEST') == '1', 'set RUNWAY_GARAGE_TEST=1 for real Garage')
class GarageTest(unittest.TestCase):
    def test_put_get_range_delete(self):
        import boto3
        from botocore.config import Config
        from botocore.exceptions import ClientError
        self.assertEqual(sha256_file(GARAGE_BIN), EXPECTED_SHA256)
        with tempfile.TemporaryDirectory(prefix='runway-garage-') as directory:
            ports = {name: free_port() for name in ('s3', 'rpc', 'admin')}
            key, secret, bucket = 'GK' + secrets.token_hex(16), secrets.token_hex(32), 'cache-test'
            proc, log, _ = start_garage(directory, ports, key, secret, bucket)
            try:
                client = boto3.client('s3', endpoint_url='http://127.0.0.1:%d' % ports['s3'],
                    aws_access_key_id=key, aws_secret_access_key=secret, region_name=S3_REGION,
                    config=Config(signature_version='s3v4', s3={'addressing_style': 'path'},
                                  connect_timeout=1, read_timeout=3, retries={'total_max_attempts': 1}))
                deadline = time.monotonic() + 20
                while True:
                    try:
                        client.head_bucket(Bucket=bucket)
                        break
                    except Exception:
                        if proc.poll() is not None or time.monotonic() >= deadline:
                            raise
                        time.sleep(.1)
                data = os.urandom(2 * 1024 * 1024)
                client.put_object(Bucket=bucket, Key='prototype/test', Body=data)
                self.assertEqual(client.get_object(Bucket=bucket, Key='prototype/test')['Body'].read(), data)
                self.assertEqual(client.get_object(Bucket=bucket, Key='prototype/test', Range='bytes=17-99')['Body'].read(), data[17:100])
                client.delete_object(Bucket=bucket, Key='prototype/test')
                with self.assertRaises(ClientError) as error:
                    client.head_object(Bucket=bucket, Key='prototype/test')
                self.assertEqual(error.exception.response['ResponseMetadata']['HTTPStatusCode'], 404)
            finally:
                stop_garage(proc, log)

if __name__ == '__main__':
    unittest.main()
