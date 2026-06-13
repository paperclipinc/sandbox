from __future__ import annotations

import base64
import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

from mitos.sandbox import Sandbox


def _ndjson_lines():
    return [
        json.dumps({"stream": "stdout", "data": base64.b64encode(b"out1").decode()}),
        json.dumps({"stream": "stderr", "data": base64.b64encode(b"err1").decode()}),
        json.dumps({"stream": "stdout", "data": base64.b64encode(b"out2").decode()}),
        json.dumps({"exit_code": 7, "exec_time_ms": 2.0}),
    ]


class _Handler(BaseHTTPRequestHandler):
    def do_POST(self):  # noqa: N802
        length = int(self.headers.get("Content-Length", 0))
        self.rfile.read(length)
        self.send_response(200)
        self.send_header("Content-Type", "application/x-ndjson")
        self.end_headers()
        for line in _ndjson_lines():
            self.wfile.write((line + "\n").encode())
            self.wfile.flush()

    def log_message(self, *args):  # silence
        pass


@pytest.fixture()
def stream_server():
    srv = HTTPServer(("127.0.0.1", 0), _Handler)
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    yield f"127.0.0.1:{srv.server_address[1]}"
    srv.shutdown()


def _direct_sandbox(endpoint: str) -> Sandbox:
    # Build a Sandbox without k8s: set endpoint and id directly.
    sb = Sandbox.__new__(Sandbox)
    import httpx

    sb._endpoint = endpoint
    sb._sandbox_id = "sb1"
    sb._token = None
    sb._http = httpx.Client(timeout=30.0)
    return sb


def test_exec_streams_callbacks(stream_server):
    sb = _direct_sandbox(stream_server)
    out, err = [], []
    result = sb.exec(
        "echo hi",
        on_stdout=lambda b: out.append(b),
        on_stderr=lambda b: err.append(b),
    )
    assert b"".join(out) == b"out1out2"
    assert b"".join(err) == b"err1"
    assert result.exit_code == 7
    assert result.stdout == "out1out2"


def test_exec_background_wait(stream_server):
    sb = _direct_sandbox(stream_server)
    proc = sb.exec_background("sleep 1")
    result = proc.wait()
    assert result.exit_code == 7
