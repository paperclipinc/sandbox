from __future__ import annotations

import asyncio
import base64
import json
import threading
from typing import Callable, Optional

import websockets  # the async websockets package
from websocket import WebSocketApp  # from the websocket-client package


class PtyHandle:
    """A live interactive pseudo-terminal in a sandbox, mirroring E2B's
    sandbox.pty handle. Output bytes are delivered to on_data on a background
    reader thread; send_input/resize write frames to the guest, kill() force
    closes, and wait() blocks for the exit code.

    The transport is a WebSocket to forkd's GET /v1/pty (subprotocol
    mitos.pty.v1), gated by the per-sandbox bearer token. The token is sent in
    the Authorization header and is never logged.
    """

    def __init__(
        self,
        url: str,
        token: Optional[str],
        on_data: Callable[[bytes], None],
    ):
        self._on_data = on_data
        self._exit_code: Optional[int] = None
        self._done = threading.Event()
        self._open = threading.Event()
        self._lock = threading.Lock()

        header = [f"Authorization: Bearer {token}"] if token else []
        self._ws = WebSocketApp(
            url,
            header=header,
            subprotocols=["mitos.pty.v1"],
            on_open=self._handle_open,
            on_message=self._handle_message,
            on_close=self._handle_close,
            on_error=self._handle_error,
        )
        self._thread = threading.Thread(target=self._ws.run_forever, daemon=True)
        self._thread.start()
        # Block until the socket is open so the first send_input is not dropped.
        self._open.wait(timeout=10)

    def _handle_open(self, ws) -> None:  # noqa: ANN001
        self._open.set()

    def _handle_message(self, ws, message) -> None:  # noqa: ANN001
        frame = json.loads(message)
        kind = frame.get("kind")
        if kind == "output":
            data = frame.get("data")
            decoded = base64.b64decode(data) if data else b""
            self._on_data(decoded)
        elif kind == "exit":
            self._exit_code = int(frame.get("exit_code", 0))
            self._done.set()
            ws.close()

    def _handle_close(self, ws, status_code, msg) -> None:  # noqa: ANN001
        if self._exit_code is None:
            self._exit_code = -1
        self._done.set()

    def _handle_error(self, ws, error) -> None:  # noqa: ANN001
        # Error text never carries the token; record nothing sensitive.
        if self._exit_code is None:
            self._exit_code = -1
        self._done.set()

    def _send(self, frame: dict) -> None:
        with self._lock:
            self._ws.send(json.dumps(frame))

    def send_input(self, data: bytes) -> None:
        """Send raw keystroke bytes to the shell."""
        self._send({"kind": "input", "data": base64.b64encode(data).decode("ascii")})

    def resize(self, cols: int, rows: int) -> None:
        """Resize the terminal window (TIOCSWINSZ in the guest, then SIGWINCH)."""
        self._send({"kind": "resize", "cols": cols, "rows": rows})

    def kill(self) -> None:
        """Force-close the terminal. The guest kills the shell process group
        when the connection drops."""
        try:
            self._ws.close()
        finally:
            self._done.set()

    def wait(self, timeout: Optional[float] = None) -> int:
        """Block until the shell exits and return its exit code (or -1 if the
        connection dropped before a terminal exit frame)."""
        self._done.wait(timeout=timeout)
        return self._exit_code if self._exit_code is not None else -1


class AsyncPtyHandle:
    """Async counterpart to PtyHandle. Output is delivered to on_data from a
    background asyncio task; send_input/resize are coroutines, wait() awaits the
    exit code. Transport is a WebSocket to /v1/pty gated by the bearer token."""

    def __init__(self, ws, on_data: Callable[[bytes], None]):
        self._ws = ws
        self._on_data = on_data
        self._exit_code: Optional[int] = None
        self._done = asyncio.Event()
        self._reader = asyncio.create_task(self._read_loop())

    @classmethod
    async def connect(
        cls,
        url: str,
        token: Optional[str],
        on_data: Callable[[bytes], None],
    ) -> "AsyncPtyHandle":
        headers = [("Authorization", f"Bearer {token}")] if token else []
        ws = await websockets.connect(
            url,
            additional_headers=headers,
            subprotocols=["mitos.pty.v1"],
        )
        return cls(ws, on_data)

    async def _read_loop(self) -> None:
        try:
            async for raw in self._ws:
                frame = json.loads(raw)
                kind = frame.get("kind")
                if kind == "output":
                    data = frame.get("data")
                    self._on_data(base64.b64decode(data) if data else b"")
                elif kind == "exit":
                    self._exit_code = int(frame.get("exit_code", 0))
                    break
        finally:
            if self._exit_code is None:
                self._exit_code = -1
            self._done.set()
            await self._ws.close()

    async def send_input(self, data: bytes) -> None:
        await self._ws.send(
            json.dumps({"kind": "input", "data": base64.b64encode(data).decode("ascii")})
        )

    async def resize(self, cols: int, rows: int) -> None:
        await self._ws.send(json.dumps({"kind": "resize", "cols": cols, "rows": rows}))

    async def kill(self) -> None:
        await self._ws.close()
        self._done.set()

    async def wait(self) -> int:
        await self._done.wait()
        return self._exit_code if self._exit_code is not None else -1
