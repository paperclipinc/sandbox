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

    def test_timeout_interrupts_and_kernel_survives(self):
        # A runaway cell that exceeds a short timeout must report TimeoutError
        # with a status:error done, and the kernel must still run the NEXT cell,
        # proving it was interrupted rather than left wedged on the busy loop.
        events, _ = self._run([
            {"id": "a", "code": "while True:\n    pass", "timeout": 2},
            {"id": "b", "code": "21 * 2"},
        ])
        err = next(e for e in events if e["kind"] == "error" and e["id"] == "a")
        self.assertEqual(err["name"], "TimeoutError")
        done_a = next(e for e in events if e["kind"] == "done" and e["id"] == "a")
        self.assertEqual(done_a["status"], "error")
        result_b = next(e for e in events if e["kind"] == "result" and e["id"] == "b")
        self.assertEqual(result_b["data"]["text/plain"].strip(), "42")
        done_b = next(e for e in events if e["kind"] == "done" and e["id"] == "b")
        self.assertEqual(done_b["status"], "ok")

    def test_kernel_death_reports_error(self):
        # A cell that kills the kernel mid-execution must report KernelDied with
        # a status:error done, not a clean status:ok. os._exit(0) terminates the
        # kernel process without a normal shutdown, mimicking a crash.
        events, _ = self._run([
            {"id": "a", "code": "import os\nos._exit(0)", "timeout": 30},
        ])
        done_a = next(e for e in events if e["kind"] == "done" and e["id"] == "a")
        self.assertEqual(done_a["status"], "error")
        self.assertTrue(
            any(e["kind"] == "error" and e.get("name") == "KernelDied"
                for e in events if e["id"] == "a")
        )


if __name__ == "__main__":
    unittest.main()
