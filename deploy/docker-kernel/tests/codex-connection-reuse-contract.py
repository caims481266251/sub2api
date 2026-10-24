#!/usr/bin/env python3
"""Check an executable Codex kernel's upstream HTTP connection reuse.

Uses fabricated credentials and a loopback HTTP/1.1 upstream. No business
configuration is read or changed. A persistent reference client first verifies
the fixture. Exit 0 requires three completed kernel requests over one upstream
connection; exit 1 means the candidate kernel does not meet that contract.
Run inside the existing Linux runtime with its normal egress firewall.
"""
import argparse
import hashlib
import http.client
import http.server
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import threading
import time
import uuid


class Upstream(http.server.ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self):
        super().__init__(("127.0.0.1", 0), Handler)
        self.lock = threading.Lock()
        self.connections = 0
        self.requests = 0
        self.close_headers = 0

    def get_request(self):
        conn, address = super().get_request()
        conn.settimeout(5)
        with self.lock:
            self.connections += 1
        return conn, address

    def snapshot(self):
        with self.lock:
            return dict(connections=self.connections, requests=self.requests,
                        close_headers=self.close_headers)


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_):
        pass

    def do_POST(self):
        size = int(self.headers.get("Content-Length", "0"))
        if self.path != "/responses" or not 0 < size <= 65536:
            self.send_error(400)
            return
        self.rfile.read(size)
        with self.server.lock:
            self.server.requests += 1
            sequence = self.server.requests
            if self.headers.get("Connection", "").lower() == "close":
                self.server.close_headers += 1
        event = {"type": "response.completed", "response": {
            "id": "resp_fixture_" + str(sequence), "status": "completed",
            "object": "response", "output": [],
            "usage": {"input_tokens": 1, "output_tokens": 1}}}
        payload = ("data: " + json.dumps(event) + "\n\n").encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(payload)))
        self.send_header("Connection", "keep-alive")
        self.end_headers()
        self.wfile.write(payload)
        self.wfile.flush()


def check(binary):
    binary = Path(binary).resolve(strict=True)
    report = {"kernel_sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
              "passed": False, "rows": []}
    upstream = Upstream()
    worker = threading.Thread(target=upstream.serve_forever, daemon=True)
    worker.start()
    proc = None
    conn = None
    directory = None
    try:
        control = http.client.HTTPConnection("127.0.0.1", upstream.server_port, timeout=5)
        try:
            for _ in range(3):
                control.request("POST", "/responses", "{}")
                response = control.getresponse()
                if response.status != 200 or response.will_close:
                    raise RuntimeError("fixture_keepalive_failed")
                response.read()
        finally:
            control.close()
        control_counts = upstream.snapshot()
        report["control"] = control_counts
        if control_counts["requests"] != 3 or control_counts["connections"] != 1:
            raise RuntimeError("fixture_keepalive_failed")

        with tempfile.TemporaryDirectory(prefix="sub2-pool-contract-") as directory:
            root = Path(directory)
            credential = root / "credential.json"
            credential.write_text(json.dumps({"accounts": [{
                "id": "local-fixture", "access_token": "fabricated-not-a-token",
                "chatgpt_account_id": "local-fixture",
                "max_concurrency": 0, "rpm": 0}]}))
            credential.chmod(0o600)
            config = {"vm_id": "pool-fixture", "device_id": str(uuid.uuid4()),
                      "socket_path": str(root / "kernel.sock"),
                      "credential_path": str(credential),
                      "proxy_required": False, "proxy_url": "",
                      "internal_token": "fixture-internal-token",
                      "upstream_transport": "http", "client_features": False,
                      "analytics": False, "test_endpoints": True,
                      "chatgpt_base_url": "http://127.0.0.1:" + str(upstream.server_port),
                      "max_request_bytes": 1048576}
            config_path = root / "config.json"
            config_path.write_text(json.dumps(config))
            config_path.chmod(0o600)
            env = {k: v for k, v in os.environ.items()
                   if k.lower() not in ("http_proxy", "https_proxy", "all_proxy", "no_proxy")
                   and k not in ("KIN_CODEX_DUMP_SAMPLE", "KIN_CODEX_KERNEL_CONFIG")}
            proc = subprocess.Popen([str(binary), str(config_path)], env=env,
                                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            try:
                deadline = time.monotonic() + 5
                while not Path(config["socket_path"]).exists():
                    if proc.poll() is not None:
                        raise RuntimeError("kernel_start_failed")
                    if time.monotonic() >= deadline:
                        raise TimeoutError("kernel_start_timeout")
                    time.sleep(.05)
                sock = socket.socket(socket.AF_UNIX)
                sock.settimeout(5)
                conn = http.client.HTTPConnection("fixture", timeout=5)
                conn.sock = sock
                sock.connect(config["socket_path"])
                for index in range(3):
                    envelope = {"request_id": str(uuid.uuid4()), "stream": True,
                                "body": {"model": "gpt-6-luna", "input": "local fixture",
                                         "instructions": "local fixture", "store": False, "stream": True},
                                "session": {"session_id": "pool-fixture"},
                                "options": {"upstream_transport": "http"}}
                    conn.request("POST", "/internal/v1/codex/responses", json.dumps(envelope),
                                 {"Content-Type": "application/json",
                                  "x-kin-internal-token": config["internal_token"],
                                  "x-internal-token": config["internal_token"]})
                    response = conn.getresponse()
                    data = response.read(65537)
                    completed = False
                    for line in data.splitlines():
                        if line.startswith(b"data: "):
                            event = json.loads(line[6:])
                            completed |= (event.get("type") == "response.completed" and
                                          event.get("response", {}).get("status") == "completed")
                    row = {"turn": index + 1, "http_status": response.status,
                           "completed": completed, "uds_keepalive": not response.will_close}
                    report["rows"].append(row)
                    if response.status != 200 or not completed or response.will_close or len(data) > 65536:
                        raise RuntimeError("kernel_request_contract_failed")
                    time.sleep(.05)
                current = upstream.snapshot()
                report["kernel"] = {key: current[key] - control_counts[key] for key in current}
                report["passed"] = (report["kernel"]["requests"] == 3 and
                                    report["kernel"]["connections"] == 1)
                if not report["passed"]:
                    report["reason"] = "upstream_connection_not_reused"
            finally:
                if conn is not None:
                    conn.close()
                if proc.poll() is None:
                    proc.terminate()
                    try:
                        proc.wait(3)
                    except subprocess.TimeoutExpired:
                        proc.kill()
                        proc.wait()
    except Exception as error:
        # No raw kernel response, credential, URL or stderr is emitted.
        report["error_class"] = type(error).__name__
    finally:
        upstream.shutdown()
        upstream.server_close()
        worker.join(2)
        report["temporary_kernel_stopped"] = proc is None or proc.poll() is not None
        report["temporary_directory_removed"] = directory is None or not Path(directory).exists()
    print(json.dumps(report, sort_keys=True))
    return report["passed"]


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, help="Path to an executable Linux Codex kernel")
    args = parser.parse_args()
    raise SystemExit(0 if check(args.binary) else 1)
