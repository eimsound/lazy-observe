"""lazy-observe Python SDK: explicit Django/DRF client instrumentation.

Three-line setup:

    import lazyobserve
    lo = lazyobserve.init({"dsn": "http://host:3100/p/myapp@token", "service_name": "checkout-api"})
    # MIDDLEWARE += ["lazyobserve.DjangoMiddleware"]
    # REST_FRAMEWORK["EXCEPTION_HANDLER"] = "lazyobserve.drf.exception_handler"
"""

from . import _registry  # noqa: F401
from .client import Client, Diagnostics, init_client
from .config import Config, DeliveryConfig, InvalidConfigError
from .dsn import DSN, parse_dsn
from .django import DjangoMiddleware
from .logging import LazyObserveLogHandler
from . import drf  # noqa: F401  (module path lazyobserve.drf.exception_handler)

__version__ = "0.1.0"


def init(config: "Config | dict | None" = None, **kwargs) -> Client:
    """Create the SDK. ``config`` may be a :class:`Config`, a dict of fields, or
    keyword arguments. Invalid configuration returns a safe degraded instance;
    inspect ``client.err()`` to report the construction failure.

    The constructed client becomes the registry's active instance so that
    registry-bound integrations (``lazyobserve.DjangoMiddleware``,
    ``lazyobserve.drf.exception_handler``, ``LazyObserveLogHandler``) resolve it.
    """
    known = Config.__dataclass_fields__
    if isinstance(config, dict):
        config = Config(**{k: v for k, v in config.items() if k in known})
    elif kwargs:
        config = Config(**{k: v for k, v in kwargs.items() if k in known})
    if config is None:
        config = Config()
    client = init_client(config)
    _set_active(client)
    return client


def get_client() -> "Client | None":
    """Return the registry's active client, or None when uninitialized."""
    return _registry.get_active_client()


def _set_active(client: Client) -> None:
    _registry.set_active(client)


__all__ = [
    "Client",
    "Config",
    "DeliveryConfig",
    "DSN",
    "DjangoMiddleware",
    "InvalidConfigError",
    "LazyObserveLogHandler",
    "Diagnostics",
    "drf",
    "get_client",
    "init",
    "parse_dsn",
]