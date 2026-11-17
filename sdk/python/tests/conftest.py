import os
import sys

import django

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

os.environ.setdefault("DJANGO_SETTINGS_MODULE", "tests.django_settings")
django.setup()


import pytest  # noqa: E402

from tests.helpers import make_client  # noqa: E402


@pytest.fixture
def client():
    c = make_client()
    yield c
    c.shutdown(timeout=1.0)
