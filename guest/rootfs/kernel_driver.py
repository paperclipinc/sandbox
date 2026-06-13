#!/usr/bin/env python3
"""In-guest kernel driver for mitos run_code.

Owns one ipykernel for the sandbox lifetime and exposes a line-oriented JSON
protocol on stdin/stdout so the (Go) guest agent can drive it without speaking
ZMQ:

  stdin  (one JSON object per line):  {"id": "<exec-id>", "code": "<source>"}
  stdout (one JSON object per line):  {"id": "<exec-id>", "kind": "...", ...}

Emitted kinds, in IOPub order, terminated by exactly one "done":
  {"id","kind":"stdout","text": "..."}                 # stream name == stdout
  {"id","kind":"stderr","text": "..."}                 # stream name == stderr
  {"id","kind":"result","text": "<text/plain or ''>",  # display_data / execute_result
        "data": {"<mime>": "<payload>", ...}}           # base64 for image/png
  {"id","kind":"error","name":"...","value":"...","traceback":[...]}
  {"id","kind":"done","status":"ok|error|aborted"}

State persists across requests because it is one long-lived kernel namespace.
Only one request is processed at a time (the agent serializes; the kernel is
single threaded regardless).
"""
import base64
import json
import sys

from jupyter_client.manager import start_new_kernel

# MIME types whose payload ipykernel delivers already base64-encoded as bytes
# we keep as-is (image/png, image/jpeg). Everything else is text we pass through.
_BINARY_MIMES = {"image/png", "image/jpeg"}


def _emit(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()


def _normalize_data(data):
    """Turn a Jupyter display data dict into a {mime: str} map.

    ipykernel hands image payloads as base64 strings already; text payloads as
    str; application/json as a dict (which we re-serialize). We coerce all
    values to strings so the wire shape is uniform (matches ResultFrame.Data).
    """
    out = {}
    for mime, payload in data.items():
        if mime in _BINARY_MIMES:
            out[mime] = payload if isinstance(payload, str) else base64.b64encode(payload).decode()
        elif mime == "application/json":
            out[mime] = payload if isinstance(payload, str) else json.dumps(payload)
        elif isinstance(payload, (bytes, bytearray)):
            out[mime] = base64.b64encode(payload).decode()
        else:
            out[mime] = str(payload)
    return out


def _run_one(client, exec_id, code):
    msg_id = client.execute(code, store_history=True)
    status = "ok"
    while True:
        try:
            msg = client.get_iopub_msg(timeout=None)
        except Exception:
            break
        parent = msg.get("parent_header", {})
        if parent.get("msg_id") != msg_id:
            continue
        mtype = msg["msg_type"]
        content = msg["content"]
        if mtype == "stream":
            kind = "stdout" if content.get("name") == "stdout" else "stderr"
            _emit({"id": exec_id, "kind": kind, "text": content.get("text", "")})
        elif mtype in ("display_data", "execute_result"):
            data = _normalize_data(content.get("data", {}))
            text = data.get("text/plain", "") if mtype == "execute_result" else ""
            _emit({"id": exec_id, "kind": "result", "text": text, "data": data})
        elif mtype == "error":
            status = "error"
            _emit({
                "id": exec_id,
                "kind": "error",
                "name": content.get("ename", ""),
                "value": content.get("evalue", ""),
                "traceback": content.get("traceback", []),
            })
        elif mtype == "status" and content.get("execution_state") == "idle":
            break
    _emit({"id": exec_id, "kind": "done", "status": status})


def main():
    # start_new_kernel returns (KernelManager, BlockingKernelClient) with the
    # client already connected and channels started.
    km, client = start_new_kernel(kernel_name="python3")
    # Route matplotlib to the inline backend so figures become image/png
    # display_data instead of trying to open a GUI window.
    client.execute(
        "import matplotlib\n"
        "matplotlib.use('module://matplotlib_inline.backend_inline')\n",
        store_history=False, silent=True,
    )
    _emit({"id": "", "kind": "ready"})
    try:
        for line in sys.stdin:
            line = line.strip()
            if not line:
                continue
            req = json.loads(line)
            _run_one(client, req.get("id", ""), req.get("code", ""))
    finally:
        client.stop_channels()
        km.shutdown_kernel(now=True)


if __name__ == "__main__":
    main()
