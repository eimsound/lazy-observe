"""Django test settings used by the SDK test suite (no DB required)."""

SECRET_KEY = "lazy-observe-test-secret-key"

INSTALLED_APPS = [
    "django.contrib.contenttypes",
]

MIDDLEWARE = []

DATABASES = {
    "default": {
        "ENGINE": "django.db.backends.sqlite3",
        "NAME": ":memory:",
    }
}

USE_TZ = True

ROOT_URLCONF = "tests.urls"

ALLOWED_HOSTS = ["*"]

REST_FRAMEWORK = {
    "EXCEPTION_HANDLER": "lazy_observe.drf.exception_handler",
}

DEFAULT_AUTO_FIELD = "django.db.models.AutoField"
