from __future__ import annotations

import uuid
from typing import Optional

from kubernetes import client as k8s_client
from kubernetes import config as k8s_config

from agent_run.sandbox import Sandbox
from agent_run.types import PoolStatus, SandboxPhase


API_GROUP = "agentrun.dev"
API_VERSION = "v1alpha1"


class AgentRun:
    """Client for the agent-run sandbox runtime."""

    def __init__(
        self,
        namespace: str = "default",
        kubeconfig: Optional[str] = None,
        in_cluster: bool = False,
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
