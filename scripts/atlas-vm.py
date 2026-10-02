#!/usr/bin/env python3
"""Create, inspect or terminate one Runway dogfood VM through Central's Atlas client.

Run with the Python environment of a bench containing Central. Select the
bench, site, team and region explicitly. Termination submits at most one
DELETE, then polls for scoped 404.
"""

import argparse
import json
import os
import re
import sys
import time
from datetime import UTC, datetime
from pathlib import Path
from uuid import uuid4


def create_once(client, payload: dict, output: Path, identity: dict) -> str:
    """Reserve a durable JSONL journal before one create; never retry here."""
    descriptor = os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w") as journal:

        def record(status: str, **details: object) -> None:
            journal.write(json.dumps({"status": status, **identity, **details}) + "\n")
            journal.flush()
            os.fsync(journal.fileno())

        record("requested", payload=payload)
        directory = os.open(output.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
        try:
            response = client.create_vm(payload)
        except Exception as error:
            record("unconfirmed", error_type=type(error).__name__)
            raise
        vm_id = response.get("id")
        if not isinstance(vm_id, str) or not re.fullmatch(r"vm-[0-9]+", vm_id):
            record("unconfirmed", error_type="InvalidCreateResponse")
            raise ValueError(
                "Create response has no valid VM ID; reconcile attempt tag"
            )
        record("created", vm_id=vm_id)
        return vm_id


def emit(event: str, args: argparse.Namespace, **details: object) -> None:
    print(
        json.dumps(
            {
                "event": event,
                "vm_id": args.vm_id,
                "team": args.team,
                "region": args.region,
                "observed_at": datetime.now(UTC).isoformat(),
                **details,
            }
        ),
        flush=True,
    )


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("create", "get", "terminate"))
    parser.add_argument(
        "--vm-id", help="Explicit Atlas VM ID for get/terminate, for example vm-0000001"
    )
    parser.add_argument("--image-id", help="Explicit Atlas image ID for create")
    parser.add_argument(
        "--output", type=Path, help="New exclusive mode-0600 JSONL creation journal"
    )
    parser.add_argument(
        "--ssh-public-key",
        type=Path,
        help="SSH public key file; required for create",
    )
    parser.add_argument("--bench", type=Path, required=True)
    parser.add_argument("--site", default="central.localhost")
    parser.add_argument("--team", required=True)
    parser.add_argument("--region", required=True)
    parser.add_argument(
        "--wait-seconds",
        type=int,
        default=180,
        help="Polling budget after the single termination request (default 180)",
    )
    args = parser.parse_args()
    if args.action == "create":
        if not args.image_id or not args.output or not args.ssh_public_key or args.vm_id:
            parser.error("create requires --image-id, --output and --ssh-public-key, without --vm-id")
        args.output = args.output.absolute()
        args.ssh_public_key = args.ssh_public_key.resolve()
    elif not args.vm_id or not re.fullmatch(r"vm-[0-9]+", args.vm_id):
        parser.error("get/terminate requires an explicit vm-<digits> ID")
    if args.wait_seconds < 1:
        parser.error("Use a vm-<digits> ID and a positive wait budget")

    import frappe
    from central.errors import (
        AtlasConnectionError,
        AtlasRequestUncertain,
        AtlasResourceGone,
    )
    from central.integrations.atlas import AtlasClient

    bench = args.bench.resolve()
    os.chdir(bench / "sites")
    frappe.init(site=args.site, sites_path=str(bench / "sites"))
    try:
        frappe.connect()
        frappe.set_user("Administrator")
        client = AtlasClient.for_team(
            frappe.get_doc("Region", args.region),
            args.team,
            capability={
                "create": "server:create",
                "terminate": "server:terminate",
                "get": "server:view",
            }[args.action],
        )
        if args.action == "create":
            public_key = args.ssh_public_key.read_text().strip()
            if (
                not public_key.startswith(("ssh-ed25519 ", "ssh-rsa ", "ecdsa-sha2-"))
                or "\n" in public_key
            ):
                raise ValueError("Expected one SSH public key")
            attempt = str(uuid4())
            tags = {
                "role": "ci",
                "purpose": "dogfood",
                "team": args.team,
                "attempt": attempt,
            }
            payload = {
                "image_id": args.image_id,
                "cpu_millicores": 1000,
                "memory_mib": 2048,
                "disk_mib": 4096,
                "hostname": f"runway-{attempt[:8]}",
                "ssh_keys": [public_key],
                "sleep_after_idle_seconds": 0,
                "tags": tags,
            }
            args.vm_id = create_once(
                client,
                payload,
                args.output,
                {"team": args.team, "region": args.region, "attempt": attempt},
            )
            emit("created", args, journal=str(args.output))
            return 0
        try:
            vm = client.get_vm(args.vm_id)
        except AtlasResourceGone:
            emit("absent", args, http_status=404)
            return 0

        if args.action == "get":
            fields = (
                "id",
                "tenant_id",
                "image_id",
                "architecture",
                "status",
                "state",
                "desired_state",
                "current_state",
                "cpu_millicores",
                "memory_mib",
                "disk_mib",
                "is_termination_protected",
            )
            emit("present", args, vm={key: vm[key] for key in fields if key in vm})
            return 0

        # This spike helper must not destroy an unrelated VM in the same team.
        tags = vm.get("tags") or {}
        if (
            tags.get("role") != "ci"
            or tags.get("purpose") != "dogfood"
            or tags.get("team") != args.team
        ):
            emit(
                "refused",
                args,
                reason="VM does not carry the expected CI dogfood team tags",
            )
            return 1

        try:
            client.vm_action(args.vm_id, "terminate")
            emit("termination_accepted", args)
        except AtlasResourceGone:
            # A GET still confirms absence after a DELETE race.
            emit("termination_target_gone", args)
        except AtlasRequestUncertain:
            emit(
                "termination_uncertain",
                args,
                reason="Polling only; DELETE will not be repeated",
            )

        deadline = time.monotonic() + args.wait_seconds
        while time.monotonic() < deadline:
            try:
                client.get_vm(args.vm_id)
            except AtlasResourceGone:
                emit("deletion_confirmed", args, http_status=404)
                return 0
            except AtlasConnectionError:
                emit("verification_unavailable", args)
            time.sleep(min(3, max(0, deadline - time.monotonic())))
        emit(
            "unconfirmed",
            args,
            reason="No scoped HTTP 404 before polling deadline; inspect with get",
        )
        return 1
    except Exception as error:  # noqa: BLE001 - CLI boundary must not emit credential-bearing tracebacks.
        # Avoid raw API responses or Frappe documents, which can carry credentials.
        emit("error", args, error_type=type(error).__name__)
        return 1
    finally:
        frappe.destroy()


if __name__ == "__main__":
    sys.exit(main())
