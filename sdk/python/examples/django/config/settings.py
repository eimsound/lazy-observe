"""Django settings for the lazy-observe Python SDK example (Django + DRF)."""

import logging
import os
from pathlib import Path

import lazyobserve

BASE_DIR = Path(__file__).resolve().parent.parent

SECRET_KEY = "lazy-observe-python-example-secret"
DEBUG = False
ALLOWED_HOSTS = ["*"]

INSTALLED_APPS = [
    "django.contrib.auth",
    "django.contrib.contenttypes",
    "rest_framework",
    "app",
]

MIDDLEWARE = [
    "lazyobserve.DjangoMiddleware",
    "django.middleware.common.CommonMiddleware",
]

ROOT_URLCONF = "config.urls"

DATABASES = {
    "default": {
        "ENGINE": "django.db.backends.sqlite3",
        "NAME": BASE_DIR / "db.sqlite3",
    }
}

USE_TZ = True
DEFAULT_AUTO_FIELD = "django.db.models.AutoField"

REST_FRAMEWORK = {
    "DEFAULT_RENDERER_CLASSES": ["rest_framework.renderers.JSONRenderer"],
    "EXCEPTION_HANDLER": "lazyobserve.drf.exception_handler",
}

LOGGING = {
    "version": 1,
    "disable_existing_loggers": False,
    "handlers": {
        "lazy_observe": {
            "class": "lazyobserve.LazyObserveLogHandler",
            "formatter": "verbose",
        },
    },
    "formatters": {
        "verbose": {
            "format": "[%(asctime)s] %(levelname)s %(name)s %(message)s",
        },
    },
    "loggers": {
        "": {
            "handlers": ["lazy_observe"],
            "level": os.environ.get("LOG_LEVEL", "INFO"),
        },
        "django.server": {
            "handlers": ["lazy_observe"],
            "level": "INFO",
            "propagate": False,
        },
    },
}


def _bool_env(name: str, default: bool) -> bool:
    value = os.environ.get(name)
    if not value:
        return default
    return value.strip().lower() in ("1", "true", "yes", "on")


# --- lazy-observe bootstrap -------------------------------------------------
LO_DSN = os.environ.get("LO_DSN", "http://127.0.0.1:3100/p/default@replace-me")

lazyobserve.init(
    {
        "dsn": LO_DSN,
        "service_name": os.environ.get("LO_SERVICE_NAME", "lazy-observe-django-example"),
        "export_interval": float(os.environ.get("LO_EXPORT_INTERVAL_MS", "1000")) / 1000.0,
        "capture_llm_content": _bool_env("LO_CAPTURE_LLM_CONTENT", False),
        "capture_db_statement": _bool_env("LO_CAPTURE_DB_STATEMENT", False),
        "release": os.environ.get("LO_RELEASE", ""),
        "environment": os.environ.get("LO_ENVIRONMENT", ""),
    }
)

logging.getLogger("lazyobserve").info(
    "lazy-observe SDK initialized: dsn=%s", LO_DSN, extra={"dsn": LO_DSN}
)

# Shut the SDK down cleanly on ordinary interpreters (WSGI/management commands).
import atexit  # noqa: E402

atexit.register(lambda: lazyobserve.get_client().shutdown(timeout=5.0) if lazyobserve.get_client() else None)
