#!/usr/bin/python3 -I
"""Negative-check probe, run inside a guest as root: send one cache setup
request (JSON object on stdin) to host vsock port 1026 and print the reply.
Missing fields default to a well-formed setup; a fresh nonce is always used.
It holds no credential: the host decides from its own binding and GitHub."""
import json, socket, sys, uuid

override = json.load(sys.stdin)
context = {"repository": None, "repository_id": None, "run_id": None, "run_attempt": None, "ref": None,
           "ref_type": "branch", "sha": None, "event_name": "workflow_dispatch", "base_ref": "", "head_ref": ""}
context.update(override.pop("context", {}))
request = {"version": 2, "nonce": str(uuid.uuid4()), "context": context}
request.update(override)
with socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM) as connection:
    connection.settimeout(5)
    connection.connect((socket.VMADDR_CID_HOST, 1026))
    connection.sendall(json.dumps(request, separators=(",", ":")).encode() + b"\n")
    reply = connection.makefile("rb").readline().decode().strip()
print(json.dumps({"sent": {"run_id": context["run_id"], "repository": context["repository"]}, "reply": json.loads(reply) if reply else None}))
