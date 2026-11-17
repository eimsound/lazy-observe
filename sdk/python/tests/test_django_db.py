import pytest
from django.db import connection

from tests.helpers import last_span, make_client


@pytest.fixture
def db_ready():
    connection.ensure_connection()


def test_db_span_without_statement_capture(client, db_ready):
    with client.django_db():
        with connection.cursor() as cursor:
            cursor.execute("SELECT 1")
    span = last_span(client)
    assert span is not None
    assert span.name == "DB query"
    assert span.kind.name == "CLIENT"
    assert dict(span.attributes).get("db.system.name") == "sqlite"
    assert "db.statement" not in span.attributes


def test_db_span_with_statement_capture(db_ready):
    client = make_client(capture_db_statement=True)
    try:
        with client.django_db():
            with connection.cursor() as cursor:
                cursor.execute("SELECT 1 WHERE 1 = 1")
        span = last_span(client)
        assert span.name == "DB SELECT 1 WHERE 1 = 1"
        assert "db.statement" in dict(span.attributes)
    finally:
        client.shutdown(timeout=1.0)


def test_db_span_wrapped_attributes(client, db_ready):
    with client.django_db():
        with connection.cursor() as cursor:
            cursor.execute("SELECT 42")
    span = last_span(client)
    assert dict(span.attributes)["db.system.name"] == "sqlite"
