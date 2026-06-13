from mitos.types import Execution, Result, ExecutionError


def test_result_mime_accessors():
    r = Result(data={"image/png": "aGVsbG8=", "text/plain": "fig"})
    assert r.png == "aGVsbG8="
    assert r.text == "fig"
    assert r.html is None
    assert r.svg is None


def test_execution_shape():
    ex = Execution(
        text="42",
        logs={"stdout": ["hi\n"], "stderr": []},
        results=[Result(data={"text/plain": "42"})],
        error=None,
    )
    assert ex.text == "42"
    assert ex.logs["stdout"] == ["hi\n"]
    assert ex.results[0].text == "42"
    assert ex.error is None


def test_execution_error():
    err = ExecutionError(name="ValueError", value="bad", traceback=["...", "ValueError: bad"])
    ex = Execution(text=None, logs={"stdout": [], "stderr": []}, results=[], error=err)
    assert ex.error.name == "ValueError"
    assert "ValueError: bad" in ex.error.traceback
