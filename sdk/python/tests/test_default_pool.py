from unittest import mock

import pytest

from mitos.client import AgentRun, default_pool_name
from mitos.errors import AgentRunError


def test_default_pool_name_slug():
    assert default_pool_name("python") == "mitos-default-python"
    assert default_pool_name("python:3.12-slim") == "mitos-default-python-3.12-slim"
    assert default_pool_name("Python") == "mitos-default-python"  # lowercased
    # Slug is bounded to 40 chars after the prefix.
    long = default_pool_name("ghcr.io/paperclipinc/agent-python-with-a-very-long-tag:3.12")
    assert long.startswith("mitos-default-")
    assert len(long[len("mitos-default-"):]) <= 40


def test_default_pool_name_strips_trailing_dot():
    # A trailing "." is an invalid object-name tail and must be stripped.
    assert default_pool_name("python.") == "mitos-default-python"
    assert default_pool_name("python-") == "mitos-default-python"


def _agentrun_with_fake_api():
    c = AgentRun.__new__(AgentRun)  # bypass kube config loading
    c._api = mock.MagicMock()
    c._core_api = mock.MagicMock()
    c._namespace = "default"
    c._allow_default_pool = True
    return c


def test_sandbox_image_creates_pool_when_missing():
    c = _agentrun_with_fake_api()
    # get_namespaced_custom_object raises 404 -> template+pool absent -> create.
    from kubernetes.client.rest import ApiException
    c._api.get_namespaced_custom_object.side_effect = ApiException(status=404)

    with mock.patch.object(AgentRun, "create") as create:
        c.sandbox(image="python")
        # The created objects: a SandboxTemplate (spec.image) and a SandboxPool
        # referencing it (spec.templateRef).
        bodies = [kw["body"] for _, kw in c._api.create_namespaced_custom_object.call_args_list]
        kinds = {b["kind"]: b for b in bodies}
        assert "SandboxTemplate" in kinds
        assert "SandboxPool" in kinds
        assert kinds["SandboxTemplate"]["metadata"]["name"] == "mitos-default-python"
        assert kinds["SandboxTemplate"]["spec"]["image"] == "python"
        assert kinds["SandboxPool"]["metadata"]["name"] == "mitos-default-python"
        assert kinds["SandboxPool"]["spec"]["templateRef"]["name"] == "mitos-default-python"
        # And a claim was created from that pool.
        create.assert_called_once()
        assert create.call_args.kwargs["pool"] == "mitos-default-python"


def _get_object_router(pool_image):
    """Routes get_namespaced_custom_object by plural: an existing default pool
    whose templateRef points at a SandboxTemplate carrying pool_image."""
    name = "mitos-default-python"

    def _get(**kwargs):
        plural = kwargs["plural"]
        if plural == "sandboxpools":
            return {"metadata": {"name": name}, "spec": {"templateRef": {"name": name}}}
        if plural == "sandboxtemplates":
            return {"metadata": {"name": name}, "spec": {"image": pool_image}}
        raise AssertionError(f"unexpected plural {plural}")

    return _get


def test_sandbox_image_reuses_existing_pool():
    c = _agentrun_with_fake_api()
    c._api.get_namespaced_custom_object.side_effect = _get_object_router("python")
    with mock.patch.object(AgentRun, "create") as create:
        c.sandbox(image="python")
        c._api.create_namespaced_custom_object.assert_not_called()  # pool reused
        create.assert_called_once()


def test_sandbox_image_reuse_with_colliding_slug_raises_mismatch():
    # image A ("python-3.11") owns the default pool; calling with image B
    # ("python:3.11") collides to the same slug and must NOT silently reuse.
    assert default_pool_name("python:3.11") == default_pool_name("python-3.11")
    c = _agentrun_with_fake_api()
    c._api.get_namespaced_custom_object.side_effect = _get_object_router("python-3.11")
    with mock.patch.object(AgentRun, "create") as create:
        with pytest.raises(AgentRunError) as ei:
            c.sandbox(image="python:3.11")
        assert ei.value.code == "pool_image_mismatch"
        assert ei.value.remediation
        create.assert_not_called()  # no sandbox created


def test_sandbox_image_reuse_fails_closed_when_template_unreadable():
    from kubernetes.client.rest import ApiException

    c = _agentrun_with_fake_api()

    def _get(**kwargs):
        if kwargs["plural"] == "sandboxpools":
            return {"metadata": {"name": "mitos-default-python"}, "spec": {"templateRef": {"name": "mitos-default-python"}}}
        raise ApiException(status=404)

    c._api.get_namespaced_custom_object.side_effect = _get
    with mock.patch.object(AgentRun, "create"):
        with pytest.raises(AgentRunError) as ei:
            c.sandbox(image="python")
        assert ei.value.code == "pool_image_mismatch"


def test_explicit_pool_never_creates():
    c = _agentrun_with_fake_api()
    with mock.patch.object(AgentRun, "create") as create:
        c.sandbox(pool="my-pool")
        c._api.get_namespaced_custom_object.assert_not_called()
        c._api.create_namespaced_custom_object.assert_not_called()
        assert create.call_args.kwargs["pool"] == "my-pool"


def test_opt_out_raises_without_pool():
    c = _agentrun_with_fake_api()
    c._allow_default_pool = False
    with pytest.raises(AgentRunError) as ei:
        c.sandbox(image="python")
    assert ei.value.code == "no_default_pool"
    assert ei.value.remediation


def test_sandbox_requires_image_or_pool():
    c = _agentrun_with_fake_api()
    with pytest.raises(AgentRunError) as ei:
        c.sandbox()
    assert ei.value.code == "missing_image_or_pool"
