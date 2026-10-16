"""Safely probe the public TRAE-009 CLI without real browsers or credentials."""
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile

SCRATCH = os.environ.get("TMPDIR") or tempfile.gettempdir()
CASES = [
    (["--help"], True),
    (["api", "--help"], True),
    (["login", "--help"], True),
    (["--version"], True),
    (["auth"], True),
    (["auth", "--help"], True),
    (["unknown"], True),
]


def main():
    if len(sys.argv) != 3:
        raise SystemExit("usage: python3 cli_probe.py BINARY RESULT.json")
    binary, output = sys.argv[1:]
    rows = []
    with tempfile.TemporaryDirectory(dir=SCRATCH) as directory, socket.socket() as api, socket.socket() as auth:
        home = Path(directory) / "home"
        home.mkdir(mode=0o700)
        for held in (api, auth):
            held.bind(("127.0.0.1", 0))
            held.listen()
        marker = Path(directory) / "opener_called"
        opener = Path(directory) / "open"
        opener.write_text("#!/bin/sh\ntouch " + str(marker) + "\nexit 97\n", encoding="utf-8")
        opener.chmod(0o700)
        env = {key: value for key, value in os.environ.items() if not key.startswith("TRAE_API_")}
        env.update(
            HOME=str(home),
            TRAE_API_ADDR=f"127.0.0.1:{api.getsockname()[1]}",
            TRAE_API_AUTH_ADDR=f"127.0.0.1:{auth.getsockname()[1]}",
            TRAE_API_TOKEN="synthetic-gateway",
            PATH=directory + os.pathsep + env.get("PATH", ""),
        )
        for args, safe in CASES:
            proc = subprocess.run([binary, *args], env=env, capture_output=True, text=True, timeout=6, check=False)
            assert not marker.exists(), f"{args} attempted browser open"
            assert "login_trace_id" not in proc.stdout + proc.stderr, "authorization link leaked"
            row = {"args": args, "exit": proc.returncode, "stdout_bytes": len(proc.stdout), "stderr_bytes": len(proc.stderr)}
            if safe:
                assert args not in (["auth"], ["auth", "--help"]) or proc.returncode != 0, "removed auth command still public"
            rows.append(row)
    with open(output, "w", encoding="utf-8") as handle:
        json.dump(rows, handle, indent=2)
        handle.write("\n")
    for row in rows:
        print(row)


if __name__ == "__main__":
    main()
