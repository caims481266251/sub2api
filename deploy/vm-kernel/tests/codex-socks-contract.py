#!/usr/bin/env python3
"""Check the fixed Codex kernel against a local SOCKS server with no forwarding.

Uses only fabricated credentials, a private temporary config/socket, and a
loopback SOCKS listener. The listener rejects CONNECT after recording protocol
metadata, so no TLS request or model request can reach an upstream provider.
"""
import argparse
import http.client
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import threading
import time
import uuid


def exact(conn, n):
    result = b""
    while len(result) < n:
        chunk = conn.recv(n - len(result))
        if not chunk:
            raise EOFError()
        result += chunk
    return result


def check(binary):
    observations = []
    errors = []
    listener = socket.socket()
    listener.bind(("127.0.0.1", 0))
    listener.listen(4)
    listener.settimeout(15)

    def socks():
        try:
            conn, _ = listener.accept()
            with conn:
                conn.settimeout(10)
                version, count = exact(conn, 2)
                methods = exact(conn, count)
                if version != 5 or 2 not in methods:
                    raise ValueError("username_auth_not_offered")
                conn.sendall(b"\x05\x02")
                av, n = exact(conn, 2)
                user = exact(conn, n)
                n = exact(conn, 1)[0]
                password = exact(conn, n)
                auth_ok = av == 1 and user == b"fixture-user" and password == b"fixture-pass"
                conn.sendall(b"\x01\x00" if auth_ok else b"\x01\x01")
                version, command, reserved, atyp = exact(conn, 4)
                if atyp == 3:
                    target = exact(conn, exact(conn, 1)[0])
                elif atyp == 1:
                    target = exact(conn, 4)
                elif atyp == 4:
                    target = exact(conn, 16)
                else:
                    raise ValueError("invalid_address_type")
                port = int.from_bytes(exact(conn, 2), "big")
                observations.append({"socks_version": version, "command": command,
                                     "authentication_ok": auth_ok, "address_type": atyp,
                                     "target_is_chatgpt": target in (b"chatgpt.com", b"chat.openai.com"),
                                     "target_port": port, "upstream_forwarded": False})
                # General SOCKS failure: stop before TLS/upstream work.
                conn.sendall(b"\x05\x01\x00\x01\x00\x00\x00\x00\x00\x00")
        except Exception as error:
            errors.append(type(error).__name__)
        finally:
            listener.close()

    worker = threading.Thread(target=socks, daemon=True)
    worker.start()
    with tempfile.TemporaryDirectory(prefix="sub2-socks-contract-") as directory:
        root = Path(directory)
        (root / "credential.json").write_text(json.dumps({"accounts": [{"id": "fixture", "access_token": "fixture-access-token", "chatgpt_account_id": "fixture-account"}]}))
        config = {"vm_id": "socks-contract", "device_id": str(uuid.uuid4()),
                  "socket_path": str(root / "kernel.sock"),
                  "credential_path": str(root / "credential.json"),
                  "proxy_url": "socks5h://fixture-user:fixture-pass@127.0.0.1:" + str(listener.getsockname()[1]),
                  "proxy_required": True, "internal_token": "fixture-internal-token",
                  "max_request_bytes": 1048576}
        (root / "config.json").write_text(json.dumps(config))
        env = {k: v for k, v in os.environ.items() if k.lower() not in ("http_proxy", "https_proxy", "all_proxy", "no_proxy")}
        proc = subprocess.Popen([binary, str(root / "config.json")], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        try:
            for _ in range(100):
                if (root / "kernel.sock").exists():
                    break
                if proc.poll() is not None:
                    raise RuntimeError("fixture_kernel_exited")
                time.sleep(.05)

            class UnixHTTP(http.client.HTTPConnection):
                def connect(self):
                    self.sock = socket.socket(socket.AF_UNIX)
                    self.sock.settimeout(15)
                    self.sock.connect(str(root / "kernel.sock"))

            conn = UnixHTTP("fixture")
            payload = {"request_id": "socks-contract", "body": {"model": "gpt-6-luna", "input": "fixture", "stream": True},
                       "stream": True, "session": {"session_id": ""}, "options": {"upstream_transport": "http"}}
            conn.request("POST", "/internal/v1/codex/responses", json.dumps(payload),
                         {"Content-Type": "application/json", "x-kin-internal-token": "fixture-internal-token", "x-internal-token": "fixture-internal-token"})
            response = conn.getresponse()
            raw = response.read(65536)
            error_code = None
            try:
                candidate = json.loads(raw).get("error", {}).get("code")
                if candidate and all(c.isalnum() or c == "_" for c in candidate):
                    error_code = candidate
            except (ValueError, AttributeError):
                pass
            conn.close()
            worker.join(16)
            report = {"kernel_http_status": response.status, "error_code": error_code, "observations": observations, "errors": errors}
            print(json.dumps(report))
            return bool(observations and observations[0]["authentication_ok"] and observations[0]["address_type"] == 3 and observations[0]["target_is_chatgpt"])
        finally:
            proc.terminate()
            try:
                proc.wait(3)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()
            listener.close()


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", required=True)
    args = parser.parse_args()
    raise SystemExit(0 if check(args.binary) else 1)
