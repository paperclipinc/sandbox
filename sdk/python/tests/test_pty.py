import asyncio
import base64
import json
import threading
import time

import pytest

from mitos.pty import PtyHandle

websockets = pytest.importorskip("websockets")


class _EchoServer:
    """A local WS server that echoes input frames as output frames and exits on
    input 'exit\\n', mimicking the forkd /v1/pty protocol."""

    def __init__(self):
        self.port = None
        self._thread = None
        self._loop = None
        self._stop = None

    def start(self):
        ready = threading.Event()

        def run():
            self._loop = asyncio.new_event_loop()
            asyncio.set_event_loop(self._loop)
            self._stop = self._loop.create_future()

            async def handler(ws):
                async for raw in ws:
                    frame = json.loads(raw)
                    if frame.get("kind") == "input":
                        data = frame.get("data", "")
                        decoded = base64.b64decode(data) if data else b""
                        if decoded == b"exit\n":
                            await ws.send(json.dumps({"kind": "exit", "exit_code": 0}))
                            return
                        await ws.send(json.dumps({"kind": "output", "data": data}))

            async def main():
                # Negotiate the same subprotocol forkd's /v1/pty advertises so
                # the websocket-client handshake matches the real server.
                server = await websockets.serve(
                    handler, "127.0.0.1", 0, subprotocols=["mitos.pty.v1"]
                )
                self.port = server.sockets[0].getsockname()[1]
                ready.set()
                await self._stop

            self._loop.run_until_complete(main())

        self._thread = threading.Thread(target=run, daemon=True)
        self._thread.start()
        ready.wait(5)

    def stop(self):
        if self._loop and self._stop and not self._stop.done():
            self._loop.call_soon_threadsafe(self._stop.set_result, None)


def test_pty_echo_and_exit():
    srv = _EchoServer()
    srv.start()
    received = []
    handle = PtyHandle(
        url=f"ws://127.0.0.1:{srv.port}/v1/pty?sandbox=sb1&cols=80&rows=24",
        token=None,
        on_data=lambda b: received.append(b),
    )
    handle.send_input(b"hi-from-test\n")
    deadline = time.time() + 3
    while time.time() < deadline and b"".join(received) != b"hi-from-test\n":
        time.sleep(0.02)
    assert b"".join(received) == b"hi-from-test\n"

    handle.send_input(b"exit\n")
    assert handle.wait(timeout=3) == 0
    srv.stop()


def test_pty_resize_sends_frame():
    srv = _EchoServer()
    srv.start()
    handle = PtyHandle(
        url=f"ws://127.0.0.1:{srv.port}/v1/pty?sandbox=sb1",
        token=None,
        on_data=lambda b: None,
    )
    # Resize should not raise; the echo server ignores it.
    handle.resize(120, 40)
    handle.send_input(b"exit\n")
    assert handle.wait(timeout=3) == 0
    srv.stop()


@pytest.mark.asyncio
async def test_async_pty_echo_and_exit():
    from mitos.pty import AsyncPtyHandle

    srv = _EchoServer()
    srv.start()
    received = []
    handle = await AsyncPtyHandle.connect(
        url=f"ws://127.0.0.1:{srv.port}/v1/pty?sandbox=sb1",
        token=None,
        on_data=lambda b: received.append(b),
    )
    await handle.send_input(b"async-hi\n")
    for _ in range(150):
        if b"".join(received) == b"async-hi\n":
            break
        await asyncio.sleep(0.02)
    assert b"".join(received) == b"async-hi\n"
    await handle.send_input(b"exit\n")
    assert await handle.wait() == 0
    srv.stop()
