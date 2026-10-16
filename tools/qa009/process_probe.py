"""Real-process login lifecycle with a synthetic browser and isolated HOME.

Only loopback requests and synthetic credentials. Authorization URL/state stay in
memory, never in the result JSON or terminal output.
"""
import json
import os
from pathlib import Path
import re
import signal
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

SCRATCH = os.environ.get("TMPDIR") or tempfile.gettempdir()


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def listeners(pid):
    result = subprocess.run(
        ["/usr/sbin/lsof", "-a", "-nP", "-p", str(pid), "-iTCP", "-sTCP:LISTEN", "-F", "n"],
        capture_output=True, text=True, check=False,
    )
    return sorted({(match.group(1), int(match.group(2))) for match in re.finditer(r"^n(.+):(\d+)$", result.stdout, re.M)})


def wait_ports(proc, ports, timeout=5):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            raise AssertionError("service exited before expected listeners")
        actual = listeners(proc.pid)
        if {p for _, p in actual} == set(ports) and len(actual) == len(ports):
            time.sleep(0.15)
            actual = listeners(proc.pid)
            assert {p for _, p in actual} == set(ports) and len(actual) == len(ports), "extra listener"
            assert all(host == "127.0.0.1" for host, _ in actual), "non-loopback bind"
            return
        time.sleep(0.05)
    raise AssertionError("expected listener set not ready")


def stop(proc):
    if proc.poll() is None:
        proc.send_signal(signal.SIGINT)
    try:
        stdout, stderr = proc.communicate(timeout=5)
    except subprocess.TimeoutExpired:
        proc.kill()
        stdout, stderr = proc.communicate(timeout=5)
        raise AssertionError("process ignored interrupt and required kill")
    assert not listeners(proc.pid), "listener survived process exit"
    # Never log raw output: a failure fallback may contain the official link.
    return proc.returncode, stdout, stderr


def http_request(port, path, *, token=False, headers=None):
    target = urllib.parse.urlsplit(path)
    assert target.scheme == target.netloc == "" and path.startswith("/"), "non-local path"
    h = dict(headers or {})
    if token:
        h["Authorization"] = "Bearer synthetic-gateway"
    req = urllib.request.Request(f"http://127.0.0.1:{port}{path}", headers=h)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPHandler(), NoRedirect())
    with opener.open(req, timeout=3) as response:
        return response.status, response.read()


def opener_stub(directory):
    path = Path(directory) / "open"
    path.write_text(
        "#!/usr/bin/python3\nimport os,socket,sys\n"
        "with socket.create_connection(('127.0.0.1',int(os.environ['QA_OPEN_PORT'])),timeout=2) as s:\n"
        "    s.sendall(sys.argv[1].encode())\n",
        encoding="utf-8",
    )
    path.chmod(0o700)
    return path


def base_env(home, stub_dir, opener_port, api_port, auth_port):
    env = {key: value for key, value in os.environ.items() if not key.startswith("TRAE_API_")}
    env.update(
        HOME=str(home),
        GOCACHE="/Users/zhoujun/Library/Caches/go-build",
        GOPATH="/Users/zhoujun/go",
        TRAE_API_ADDR=f"127.0.0.1:{api_port}",
        TRAE_API_AUTH_ADDR=f"127.0.0.1:{auth_port}",
        TRAE_API_TOKEN="synthetic-gateway",
        QA_OPEN_PORT=str(opener_port),
        PATH=str(stub_dir) + os.pathsep + env.get("PATH", ""),
    )
    return env


def run(binary):
    result = {}
    with tempfile.TemporaryDirectory(dir=SCRATCH) as directory:
        root = Path(directory)
        home = root / "isolated-home"
        home.mkdir(mode=0o700)
        opener_stub(root)
        api_port, auth_port = free_port(), free_port()
        assert api_port != auth_port
        collector = socket.socket()
        collector.bind(("127.0.0.1", 0))
        collector.listen()
        collector.settimeout(5)
        env = base_env(home, root, collector.getsockname()[1], api_port, auth_port)
        login = subprocess.Popen([binary, "login"], env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            conn, _ = collector.accept()
            with conn:
                link = conn.recv(32768).decode()
            wait_ports(login, [auth_port])
            official = urllib.parse.urlsplit(link)
            assert official.scheme == "https" and official.hostname == "www.trae.cn", "nonofficial browser target"
            callback = urllib.parse.urlsplit(urllib.parse.parse_qs(official.query)["auth_callback_url"][0])
            assert callback.hostname == "127.0.0.1" and callback.port == auth_port and callback.path == "/authorize"
            query = urllib.parse.urlencode({
                "loginTraceID": urllib.parse.parse_qs(official.query)["login_trace_id"][0],
                "userJwt": json.dumps({
                    "Token": "synthetic-access",
                    "RefreshToken": "synthetic-refresh",
                    "TokenExpireAt": int(time.time()) + 3600,
                    "RefreshExpireAt": int(time.time()) + 86400,
                }),
                "userInfo": json.dumps({"UserID": "synthetic-user", "Region": "CN"}),
            })
            status, _ = http_request(auth_port, "/authorize?" + query, headers={
                "Origin": official.scheme + "://" + official.netloc,
                "Sec-Fetch-Site": "cross-site",
            })
            assert status == 200, "synthetic callback failed"
            path = home / "Library" / "Application Support" / "trae-api" / "auth.json"
            deadline = time.monotonic() + 4
            while time.monotonic() < deadline and not path.is_file():
                time.sleep(0.05)
            assert path.is_file() and path.stat().st_mode & 0o777 == 0o600, "private credential file absent"
            saved = json.loads(path.read_text())
            assert saved.get("token") == "synthetic-access", "wrong access snapshot"
            assert login.poll() is None, "self-started Auth exited after login"
            wait_ports(login, [auth_port])
            result["self_start_foreground"] = True
            # API-only must remain read-only and use the persisted access snapshot.
            before = path.read_bytes()
            before_stat = (path.stat().st_ino, path.stat().st_mtime_ns)
            api = subprocess.Popen([binary, "api"], env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            try:
                wait_ports(api, [api_port])
                status, body = http_request(api_port, "/healthz", token=True)
                assert status == 200 and json.loads(body).get("logged_in") is True
                assert path.read_bytes() == before, "API changed Auth credential file"
                assert (path.stat().st_ino, path.stat().st_mtime_ns) == before_stat, "API rewrote the credential file"
                result["api_only_read_snapshot"] = True
            finally:
                code, _, _ = stop(api)
                assert code == 0, "API-only did not close cleanly"
        finally:
            collector.close()
            code, out, _ = stop(login)
            assert code == 0, "self-started login did not close cleanly"
            assert "synthetic-access" not in out and "synthetic-refresh" not in out, "credential leaked to CLI"
        result["self_start_released"] = True
        # A separate combined process owns Auth; login verifies and reuses it.
        combined = subprocess.Popen([binary], env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            wait_ports(combined, [api_port, auth_port])
            reuse = subprocess.Popen([binary, "login"], env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            try:
                out, _ = reuse.communicate(timeout=5)
            except subprocess.TimeoutExpired:
                reuse.kill()
                reuse.communicate(timeout=5)
                raise AssertionError("verified Auth reuse did not exit")
            code = reuse.returncode
            assert not listeners(reuse.pid), "reuse process retained a listener"
            assert code == 0 and "Already logged in." in out, "verified Auth was not reused"
            assert combined.poll() is None, "reuse terminated original Auth"
            wait_ports(combined, [api_port, auth_port])
            result["existing_auth_reused"] = True
        finally:
            code, _, _ = stop(combined)
            assert code == 0, "combined process did not close cleanly"
        # Another program occupies Auth's port, with no matching writer instance.
        other_home = root / "occupied-home"
        other_home.mkdir(mode=0o700)
        occupied_env = base_env(other_home, root, env["QA_OPEN_PORT"], api_port, auth_port)
        with socket.socket() as foreign:
            foreign.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            foreign.bind(("127.0.0.1", auth_port))
            foreign.listen()
            denied = subprocess.Popen([binary, "login"], env=occupied_env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            try:
                stdout, stderr = denied.communicate(timeout=5)
            except subprocess.TimeoutExpired:
                denied.kill()
                denied.communicate(timeout=5)
                raise AssertionError("foreign-port login hung")
            assert denied.returncode != 0 and "occupied" in stderr.lower(), "foreign port was accepted"
            assert not listeners(denied.pid), "foreign-port login left a listener"
            result["foreign_port_rejected"] = True
    return result


if __name__ == "__main__":
    if len(sys.argv) != 3:
        raise SystemExit("usage: python3 process_probe.py BINARY RESULT.json")
    result = run(sys.argv[1])
    with open(sys.argv[2], "w", encoding="utf-8") as handle:
        json.dump(result, handle, indent=2)
        handle.write("\n")
    print(result)
