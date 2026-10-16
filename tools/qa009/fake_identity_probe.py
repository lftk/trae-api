"""Reject a forged Auth sidecar and fake local health responder."""
import fcntl
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading

from process_probe import SCRATCH, base_env, free_port, opener_stub


class FakeHealth(BaseHTTPRequestHandler):
    gets = 0
    posts = 0

    def do_GET(self):
        type(self).gets += 1
        data = b'{"status":"ok","logged_in":true}'
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        type(self).posts += 1
        self.send_response(200)
        self.end_headers()

    def log_message(self, format: str, *args: object) -> None:
        pass


def run(binary):
    with tempfile.TemporaryDirectory(dir=SCRATCH) as directory:
        root = Path(directory)
        home = root / "isolated-home"
        home.mkdir(mode=0o700)
        opener_stub(root)
        api_port, auth_port = free_port(), free_port()
        assert api_port != auth_port
        auth_dir = home / "Library" / "Application Support" / "trae-api"
        auth_dir.mkdir(parents=True, mode=0o700)
        lock_path = auth_dir / "auth.json.lock"
        lock_path.write_text(json.dumps({
            "version": 1,
            "address": f"127.0.0.1:{auth_port}",
            "key": (b"k" * 32).hex(),
        }))
        lock_path.chmod(0o600)
        with lock_path.open("r+b") as lock:
            fcntl.flock(lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
            FakeHealth.gets = FakeHealth.posts = 0
            server = HTTPServer(("127.0.0.1", auth_port), FakeHealth)
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                env = base_env(home, root, free_port(), api_port, auth_port)
                proc = subprocess.run([binary, "login"], env=env, capture_output=True, text=True, timeout=5, check=False)
                assert proc.returncode != 0 and "not verified" in proc.stderr, "fake instance accepted"
                assert FakeHealth.gets >= 1 and FakeHealth.posts == 0, "unverified Auth received login request"
                return {"fake_health_requested": FakeHealth.gets >= 1, "fake_login_calls": FakeHealth.posts, "rejected": True}
            finally:
                server.shutdown()
                server.server_close()
                thread.join(timeout=2)
                fcntl.flock(lock.fileno(), fcntl.LOCK_UN)


if __name__ == "__main__":
    if len(sys.argv) != 3:
        raise SystemExit("usage: python3 fake_identity_probe.py BINARY RESULT.json")
    result = run(sys.argv[1])
    with open(sys.argv[2], "w", encoding="utf-8") as handle:
        json.dump(result, handle, indent=2)
        handle.write("\n")
    print(result)
