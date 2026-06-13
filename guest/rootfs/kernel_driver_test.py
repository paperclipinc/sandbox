"""Drives kernel_driver.py over its stdin/stdout JSON protocol.

Skips entirely if ipykernel/jupyter_client are not importable, so it does not
fail on a machine without the kernel deps; the real-VM proof is Task 11.
"""
import json
import os
import subprocess
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
DRIVER = os.path.join(HERE, "kernel_driver.py")


def _have_kernel():
    try:
        import ipykernel  # noqa: F401
        import jupyter_client  # noqa: F401
        return True
    except Exception:
        return False


@unittest.skipUnless(_have_kernel(), "ipykernel/jupyter_client not installed")
class KernelDriverTest(unittest.TestCase):
    def _run(self, requests):
        proc = subprocess.Popen(
            [sys.executable, DRIVER],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            text=True,
        )
        payload = "".join(json.dumps(r) + "\n" for r in requests)
        out, err = proc.communicate(payload, timeout=120)
        events = [json.loads(line) for line in out.splitlines() if line.strip()]
        return events, err

    def test_stdout_and_last_expression(self):
        events, _ = self._run([{"id": "a", "code": "print('hi')\n40 + 2"}])
        kinds = [e["kind"] for e in events if e["id"] == "a"]
        self.assertIn("stdout", kinds)
        self.assertIn("result", kinds)
        self.assertIn("done", kinds)
        stdout = next(e for e in events if e["kind"] == "stdout")
        self.assertIn("hi", stdout["text"])
        result = next(e for e in events if e["kind"] == "result")
        self.assertEqual(result["data"]["text/plain"].strip(), "42")

    def test_state_persists(self):
        events, _ = self._run([
            {"id": "a", "code": "x = 7"},
            {"id": "b", "code": "x * 6"},
        ])
        result = next(e for e in events if e["kind"] == "result" and e["id"] == "b")
        self.assertEqual(result["data"]["text/plain"].strip(), "42")

    def test_error_frame(self):
        events, _ = self._run([{"id": "a", "code": "raise ValueError('bad')"}])
        err = next(e for e in events if e["kind"] == "error")
        self.assertEqual(err["name"], "ValueError")
        self.assertEqual(err["value"], "bad")
        self.assertTrue(any("ValueError" in line for line in err["traceback"]))


if __name__ == "__main__":
    unittest.main()
