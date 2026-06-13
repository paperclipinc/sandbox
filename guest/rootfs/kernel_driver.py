#!/usr/bin/env python3
"""In-guest kernel driver for mitos run_code.

Owns one ipykernel for the sandbox lifetime and exposes a line-oriented JSON
protocol on stdin/stdout so the (Go) guest agent can drive it without speaking
ZMQ:

  stdin  (one JSON object per line):  {"id": "<exec-id>", "code": "<source>",
                                       "timeout": <seconds, optional>}
  stdout (one JSON object per line):  {"id": "<exec-id>", "kind": "...", ...}

Emitted kinds, in IOPub order, terminated by exactly one "done":
  {"id","kind":"stdout","text": "..."}                 # stream name == stdout
  {"id","kind":"stderr","text": "..."}                 # stream name == stderr
  {"id","kind":"result","text": "<text/plain or ''>",  # display_data / execute_result
        "data": {"<mime>": "<payload>", ...}}           # base64 for image/png
  {"id","kind":"error","name":"...","value":"...","traceback":[...]}
  {"id","kind":"done","status":"ok|error|aborted"}

A request that exceeds its timeout budget interrupts the kernel (so the long
running cell is cancelled and the kernel stays usable for the next request) and
reports a TimeoutError error frame with a status:error done. A kernel that dies
mid-cell reports a KernelDied error frame with status:error rather than a clean
status:ok.

State persists across requests because it is one long-lived kernel namespace.
Only one request is processed at a time (the agent serializes; the kernel is
single threaded regardless).
"""
import base64
import json
import sys
import time

from jupyter_client.manager import start_new_kernel

# MIME types whose payload ipykernel delivers already base64-encoded as bytes
# we keep as-is (image/png, image/jpeg). Everything else is text we pass through.
_BINARY_MIMES = {"image/png", "image/jpeg"}

# Wall-clock budget applied to a single run when the request omits a positive
# timeout. Bounds a runaway cell (e.g. while True: pass) so it cannot wedge the
# kernel for every later request. Kept in sync with the Go default in
# guest/agent/kernel.go.
_DEFAULT_TIMEOUT_SECONDS = 60.0

# Poll granularity for get_iopub_msg while waiting on the kernel. Small enough
# to enforce the deadline promptly, large enough to avoid a busy loop.
_POLL_SECONDS = 0.5


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


def _drain_to_idle(km, client, msg_id, budget=10.0):
    """Consume iopub messages for msg_id until its idle status arrives, so a
    later request does not observe leftovers from an interrupted cell. Bounded
    by budget seconds and by the kernel staying alive."""
    deadline = time.monotonic() + budget
    while time.monotonic() < deadline:
        try:
            msg = client.get_iopub_msg(timeout=min(_POLL_SECONDS, deadline - time.monotonic()))
        except Exception:
            if not km.is_alive():
                return
            continue
        if msg.get("parent_header", {}).get("msg_id") != msg_id:
            continue
        if msg["msg_type"] == "status" and msg["content"].get("execution_state") == "idle":
            return


def _run_one(km, client, exec_id, code, timeout):
    """Run one cell, enforcing a wall-clock deadline.

    timeout <= 0 means use the default budget. On deadline-exceeded the kernel
    is interrupted (cancelling the running cell) and a TimeoutError error frame
    is emitted with a status:error done, so the agent's run mutex is released
    and the kernel stays usable. If get_iopub_msg raises because the kernel
    died mid-cell, a KernelDied error frame is emitted with status:error rather
    than defaulting to a misleading status:ok.
    """
    budget = timeout if timeout and timeout > 0 else _DEFAULT_TIMEOUT_SECONDS
    deadline = time.monotonic() + budget
    msg_id = client.execute(code, store_history=True)
    status = "ok"
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            # Deadline exceeded: cancel the cell so the kernel is usable again
            # rather than wedged on a runaway loop.
            try:
                km.interrupt_kernel()
            except Exception:
                pass
            # Drain this cell's interrupted execution to its idle status so the
            # next request starts on a quiet iopub channel; bounded so a kernel
            # that ignored the interrupt cannot wedge the drain.
            _drain_to_idle(km, client, msg_id)
            status = "error"
            _emit({
                "id": exec_id,
                "kind": "error",
                "name": "TimeoutError",
                "value": "execution exceeded the {:g}s timeout budget".format(budget),
                "traceback": [],
            })
            break
        try:
            msg = client.get_iopub_msg(timeout=min(_POLL_SECONDS, remaining))
        except Exception:
            # An empty queue is the normal poll-tick case: keep waiting until the
            # deadline. A dead kernel is a failure: report it instead of falling
            # through to a clean done.
            if not km.is_alive():
                status = "error"
                _emit({
                    "id": exec_id,
                    "kind": "error",
                    "name": "KernelDied",
                    "value": "the kernel terminated during execution",
                    "traceback": [],
                })
                break
            continue
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
            _run_one(km, client, req.get("id", ""), req.get("code", ""), req.get("timeout", 0))
    finally:
        client.stop_channels()
        km.shutdown_kernel(now=True)


if __name__ == "__main__":
    main()
