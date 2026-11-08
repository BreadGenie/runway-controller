#!/usr/bin/env python3
"""Disposable two-team Garage fixture for the managed Go E2E; stdout is private IPC."""
import json
import os
from pathlib import Path
import secrets
import sys
import tempfile
import time

import boto3
from botocore.config import Config
import test_garage_storage as garage


def main():
    vendor = Path(os.environ.get("RUNWAY_CACHE_VENDOR", Path(garage.GARAGE_BIN).parents[1]))
    garage.GARAGE_BIN = str(vendor / "garage-v2.3.0" / "garage")
    if garage.sha256_file(garage.GARAGE_BIN) != garage.EXPECTED_SHA256:
        raise RuntimeError("Garage pin mismatch")
    running = []
    with tempfile.TemporaryDirectory(prefix="managed-garage-") as directory:
        try:
            configs = {}
            for tenant in (2, 3):
                root = Path(directory) / str(tenant)
                root.mkdir()
                ports = {name: garage.free_port() for name in ("s3", "rpc", "admin")}
                key, secret, bucket = "GK" + secrets.token_hex(16), secrets.token_hex(32), "team-" + str(tenant)
                proc, log, _ = garage.start_garage(str(root), ports, key, secret, bucket)
                running.append((proc, log))
                endpoint = "http://127.0.0.1:" + str(ports["s3"])
                client = boto3.client("s3", endpoint_url=endpoint, aws_access_key_id=key,
                                      aws_secret_access_key=secret, region_name=garage.S3_REGION,
                                      config=Config(signature_version="s3v4", s3={"addressing_style": "path"},
                                                    connect_timeout=1, read_timeout=1,
                                                    retries={"total_max_attempts": 1}))
                deadline = time.monotonic() + 20
                while True:
                    try:
                        client.head_bucket(Bucket=bucket)
                        break
                    except Exception:
                        if proc.poll() is not None or time.monotonic() >= deadline:
                            raise RuntimeError("disposable Garage failed to become ready") from None
                        time.sleep(.05)
                configs[tenant] = dict(endpoint=endpoint, region=garage.S3_REGION, bucket=bucket,
                                       access_key=key, secret_key=secret)
            print(json.dumps(configs), flush=True)
            sys.stdin.buffer.read()  # Parent closes stdin; always reap both children.
        finally:
            for proc, log in reversed(running):
                garage.stop_garage(proc, log)


if __name__ == "__main__":
    main()
