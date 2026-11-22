"""Active-client registry so registry-bound integrations
(DjangoMiddleware, DRF handler, log handler) resolve the running SDK instance."""

from __future__ import annotations

from typing import Any, Optional

__version__ = "0.1.0"
__all__ = ["get_active_client", "set_active", "configure", "active"]


class _ClientRegistry:
    # No lock: setting and reading one attribute is atomic, with or without the GIL,
    # and a lock held by another thread at a fork would stay held in the child.
    def __init__(self) -> None:
        self._active: Optional[Any] = None

    def set_active(self, client: Optional[Any]) -> None:
        self._active = client

    def get_active_client(self) -> Optional[Any]:
        return self._active


_registry = _ClientRegistry()


def set_active(client: Optional[Any]) -> None:
    _registry.set_active(client)


def get_active_client() -> Optional[Any]:
    return _registry.get_active_client()


def configure(client: Optional[Any]) -> None:
    _registry.set_active(client)


def active() -> Optional[Any]:
    return _registry.get_active_client()
