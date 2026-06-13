from __future__ import annotations

import re
import uuid
from typing import Optional

from kubernetes import client as k8s_client
from kubernetes import config as k8s_config
from kubernetes.client.rest import ApiException

from mitos.errors import AgentRunError
from mitos.sandbox import Sandbox
from mitos.types import PoolStatus, SandboxPhase


API_GROUP = "mitos.run"
API_VERSION = "v1alpha1"

_DEFAULT_POOL_PREFIX = "mitos-default-"
_SLUG_RE = re.compile(r"[^a-z0-9.-]+")


def default_pool_name(image: str) -> str:
    """Derives a deterministic default-pool name for an image. The image is
    lowercased, "/" and ":" become "-", any other unsafe character collapses to
    "-", leading/trailing "-" and "." are stripped (a trailing "." is an invalid
    object name), and the slug is bounded so the pool name stays a valid object
    name. Kept byte-for-byte equivalent to the TypeScript defaultPoolName."""
    slug = image.lower().replace("/", "-").replace(":", "-")
    slug = _SLUG_RE.sub("-", slug)
    # Bound first, then strip trailing/leading "-" and "." so truncation can
    # never leave a name ending in "." or "-" (both invalid object-name tails).
    slug = slug[:40].strip("-.")
    return _DEFAULT_POOL_PREFIX + slug


class AgentRun:
    """Client for the mitos sandbox runtime."""

    def __init__(
        self,
        namespace: str = "default",
        kubeconfig: Optional[str] = None,
        in_cluster: bool = False,
        allow_default_pool: bool = True,
    ):
        if in_cluster:
            k8s_config.load_incluster_config()
        else:
            k8s_config.load_kube_config(config_file=kubeconfig)

        self._api = k8s_client.CustomObjectsApi()
        # Same loaded config as the CustomObjectsApi; used to read the
        # per-sandbox bearer token Secrets.
        self._core_api = k8s_client.CoreV1Api()
        self._namespace = namespace
        self._allow_default_pool = allow_default_pool

    def sandbox(
        self,
        image: Optional[str] = None,
        pool: Optional[str] = None,
        name: Optional[str] = None,
        env: Optional[dict[str, str]] = None,
        secrets: Optional[dict[str, tuple[str, str]]] = None,
        timeout: Optional[str] = None,
        ready: bool = False,
    ) -> Sandbox:
        """The one-liner entry point (docs/api/v2-spec.md section 1.2).

        Pass image= for the lazy path: the client ensures a default pool named
        mitos-default-<image-slug> exists (creating it and its SandboxTemplate
        if absent and allowed), then claims from it. Pass pool= for the explicit
        path, which never creates anything. Exactly one of image or pool is
        required.

        With ready=True the call blocks until the sandbox is Ready (or raises),
        so the caller stops sleeping-and-hoping; with ready=False (default) the
        first exec/files call lazily waits, preserving today's behavior.
        """
        if pool is None and image is None:
            raise AgentRunError(
                "sandbox() needs an image or a pool",
                code="missing_image_or_pool",
                remediation='Pass image="python" for a lazy default pool, or pool="my-pool" for an existing pool.',
            )
        if pool is None:
            if not self._allow_default_pool:
                raise AgentRunError(
                    "default pools are disabled on this client",
                    code="no_default_pool",
                    remediation="Pass pool=<name> for an existing pool, or construct AgentRun(allow_default_pool=True).",
                )
            pool = self._ensure_default_pool(image)  # type: ignore[arg-type]

        sb = self.create(
            pool=pool,
            name=name,
            env=env,
            secrets=secrets,
            timeout=timeout,
        )
        if ready:
            sb.wait_until_ready()
        return sb

    def _ensure_default_pool(self, image: str) -> str:
        """get-or-create the default SandboxPool for an image. Returns the pool
        name. A pre-existing pool is reused untouched; a missing one is created
        along with a SandboxTemplate (spec.image) it references via templateRef.

        The CRD splits image from pool: SandboxPoolSpec carries templateRef +
        replicas (api/v1alpha1/types.go), and SandboxTemplateSpec carries the
        image, so the default path materializes both objects under the same
        deterministic name."""
        name = default_pool_name(image)
        try:
            existing = self._api.get_namespaced_custom_object(
                group=API_GROUP,
                version=API_VERSION,
                namespace=self._namespace,
                plural="sandboxpools",
                name=name,
            )
            self._verify_pool_image(existing, name, image)
            return name
        except ApiException as exc:
            if exc.status != 404:
                raise

        template = {
            "apiVersion": f"{API_GROUP}/{API_VERSION}",
            "kind": "SandboxTemplate",
            "metadata": {"name": name, "namespace": self._namespace},
            "spec": {"image": image},
        }
        self._create_or_reuse(template, "sandboxtemplates")

        pool = {
            "apiVersion": f"{API_GROUP}/{API_VERSION}",
            "kind": "SandboxPool",
            "metadata": {"name": name, "namespace": self._namespace},
            "spec": {
                "templateRef": {"name": name},
                "replicas": 1,
            },
        }
        self._create_or_reuse(pool, "sandboxpools")
        return name

    def _verify_pool_image(self, pool: dict, name: str, image: str) -> None:
        """Guards the default-pool reuse path against a slug collision serving
        the wrong image. The slug normalizes ":"/"/" and other characters to
        "-", so two distinct images can map to one default pool (for example
        "python:3.11" and "python-3.11"). Reading the referenced
        SandboxTemplate's spec.image and comparing it to the requested image
        ensures a reused pool actually runs the requested image; a mismatch
        raises rather than silently running the first caller's image."""
        template_ref = (pool.get("spec") or {}).get("templateRef") or {}
        template_name = template_ref.get("name") or name
        try:
            template = self._api.get_namespaced_custom_object(
                group=API_GROUP,
                version=API_VERSION,
                namespace=self._namespace,
                plural="sandboxtemplates",
                name=template_name,
            )
        except ApiException as exc:
            # Pool with no resolvable template: cannot prove the image, so fail
            # closed rather than risk the wrong image.
            raise AgentRunError(
                f"default pool {name} references template {template_name} that could not be read",
                code="pool_image_mismatch",
                cause=f"reading SandboxTemplate {template_name} failed with status {exc.status}",
                remediation=f'Pass pool="{name}" explicitly to reuse this pool, or use a distinct image that maps to a different default pool.',
            ) from exc
        existing_image = (template.get("spec") or {}).get("image")
        if existing_image != image:
            raise AgentRunError(
                f"default pool {name} already exists for a different image",
                code="pool_image_mismatch",
                cause=f"pool {name} runs image {existing_image!r}, not the requested {image!r} (the image slug collides)",
                remediation=f'Pass pool="{name}" explicitly to reuse this pool, or use a distinct image that maps to a different default pool.',
            )

    def _create_or_reuse(self, body: dict, plural: str) -> None:
        """Create a namespaced custom object, tolerating a 409 from a concurrent
        creator (the object is reused untouched)."""
        try:
            self._api.create_namespaced_custom_object(
                group=API_GROUP,
                version=API_VERSION,
                namespace=self._namespace,
                plural=plural,
                body=body,
            )
        except ApiException as exc:
            if exc.status != 409:  # raced another creator; reuse it
                raise

    def from_name(self, name: str) -> Sandbox:
        """Reconnect to an existing sandbox by name, returning a live Sandbox
        handle (a durable handle across processes). The handle resolves its
        endpoint, phase, and per-sandbox token from the cluster; if the sandbox
        is Ready you can exec against it immediately. Alias-quality wrapper over
        get(), named for the reconnect use case."""
        return self.get(name)

    def create(
        self,
        pool: str,
        name: Optional[str] = None,
        env: Optional[dict[str, str]] = None,
        secrets: Optional[dict[str, tuple[str, str]]] = None,
        timeout: Optional[str] = None,
    ) -> Sandbox:
        """Create a sandbox from a pool.

        Args:
            pool: Name of the SandboxPool to claim from.
            name: Optional sandbox name. Generated if not provided.
            env: Environment variables to inject.
            secrets: Map of env var name to (secret_name, secret_key) tuples.
            timeout: Maximum lifetime, e.g. "30m", "1h".
        """
        if name is None:
            name = f"sandbox-{uuid.uuid4().hex[:8]}"

        claim = {
            "apiVersion": f"{API_GROUP}/{API_VERSION}",
            "kind": "SandboxClaim",
            "metadata": {
                "name": name,
                "namespace": self._namespace,
            },
            "spec": {
                "poolRef": {"name": pool},
            },
        }

        if env:
            claim["spec"]["env"] = [
                {"name": k, "value": v} for k, v in env.items()
            ]

        if secrets:
            claim["spec"]["secrets"] = [
                {
                    "name": env_var,
                    "secretRef": {"name": secret_name, "key": secret_key},
                    "envVar": env_var,
                }
                for env_var, (secret_name, secret_key) in secrets.items()
            ]

        if timeout:
            claim["spec"]["timeout"] = timeout

        self._api.create_namespaced_custom_object(
            group=API_GROUP,
            version=API_VERSION,
            namespace=self._namespace,
            plural="sandboxclaims",
            body=claim,
        )

        return Sandbox(
            name=name,
            namespace=self._namespace,
            pool=pool,
            api=self._api,
            core_api=self._core_api,
        )

    def get(self, name: str) -> Sandbox:
        """Get an existing sandbox by name."""
        obj = self._api.get_namespaced_custom_object(
            group=API_GROUP,
            version=API_VERSION,
            namespace=self._namespace,
            plural="sandboxclaims",
            name=name,
        )
        status = obj.get("status", {})
        pool = obj.get("spec", {}).get("poolRef", {}).get("name", "")

        sandbox = Sandbox(
            name=name,
            namespace=self._namespace,
            pool=pool,
            api=self._api,
            core_api=self._core_api,
            _endpoint=status.get("endpoint"),
            _phase=SandboxPhase(status.get("phase", "Pending")),
        )
        if sandbox._phase == SandboxPhase.READY:
            sandbox._load_token()
        return sandbox

    def list(self, pool: Optional[str] = None) -> list[Sandbox]:
        """List sandboxes, optionally filtered by pool."""
        objs = self._api.list_namespaced_custom_object(
            group=API_GROUP,
            version=API_VERSION,
            namespace=self._namespace,
            plural="sandboxclaims",
        )

        sandboxes = []
        for obj in objs.get("items", []):
            obj_pool = obj.get("spec", {}).get("poolRef", {}).get("name", "")
            if pool and obj_pool != pool:
                continue
            status = obj.get("status", {})
            sandboxes.append(Sandbox(
                name=obj["metadata"]["name"],
                namespace=self._namespace,
                pool=obj_pool,
                api=self._api,
                core_api=self._core_api,
                _endpoint=status.get("endpoint"),
                _phase=SandboxPhase(status.get("phase", "Pending")),
            ))
        return sandboxes

    def pool_status(self, name: str) -> PoolStatus:
        """Get the status of a SandboxPool."""
        obj = self._api.get_namespaced_custom_object(
            group=API_GROUP,
            version=API_VERSION,
            namespace=self._namespace,
            plural="sandboxpools",
            name=name,
        )
        status = obj.get("status", {})
        spec = obj.get("spec", {})
        return PoolStatus(
            name=name,
            ready_snapshots=status.get("readySnapshots", 0),
            desired=spec.get("replicas", 0),
            node_distribution=status.get("nodeDistribution", {}),
        )
