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


def test_db_records_duration_metric_even_when_query_raises(db_ready):
    from opentelemetry.sdk.metrics import MeterProvider
    from opentelemetry.sdk.metrics.export import InMemoryMetricReader

    client = make_client()
    reader = InMemoryMetricReader()
    client.meter_provider = MeterProvider(metric_readers=[reader])
    try:
        with pytest.raises(Exception):
            with client.django_db():
                with connection.cursor() as cursor:
                    cursor.execute("SELECT * FROM table_that_does_not_exist_189")
        span = last_span(client)
        assert span is not None
        assert span.status.status_code.name == "ERROR"

        data = reader.get_metrics_data()
        counts = []
        for rm in data.resource_metrics:
            for sm in rm.scope_metrics:
                for metric in sm.metrics:
                    if metric.name == "db.client.operation.duration":
                        counts.extend(p.count for p in metric.data.data_points)
        assert counts == [1]
    finally:
        client.shutdown(timeout=1.0)


def test_db_caches_histogram_across_context_entries(client, db_ready, monkeypatch):
    meter = client.meter()
    create_calls = 0
    real_create = meter.create_histogram

    def counting_create(*args, **kwargs):
        nonlocal create_calls
        create_calls += 1
        return real_create(*args, **kwargs)

    monkeypatch.setattr(client, "meter", lambda: meter)
    monkeypatch.setattr(meter, "create_histogram", counting_create)

    for _ in range(3):
        with client.django_db():
            with connection.cursor() as cursor:
                cursor.execute("SELECT 1")

    assert create_calls == 1


def test_db_handles_non_str_sql_without_type_error(db_ready):
    class _ComposedSQL:
        def __str__(self):
            return "SELECT 1 /* composed */"

    client = make_client(capture_db_statement=True)
    try:
        from lazyobserve.db import django_db_wrapper

        executed = []

        def fake_execute(sql, params, many, context):
            executed.append(sql)
            return "ok"

        with client.django_db():
            wrapper = connection.execute_wrappers[-1]
            result = wrapper(fake_execute, _ComposedSQL(), None, False, {})
        assert result == "ok"
        assert len(executed) == 1
        span = last_span(client)
        assert span is not None
        assert span.name == "DB SELECT 1 /* composed */"
        assert dict(span.attributes)["db.statement"] == "SELECT 1 /* composed */"
    finally:
        client.shutdown(timeout=1.0)


def test_db_degraded_client_yields_directly(db_ready):
    import lazyobserve

    degraded = lazyobserve.init({"dsn": "bad-dsn", "service_name": "degraded"})
    assert degraded.degraded
    with degraded.django_db():
        with connection.cursor() as cursor:
            cursor.execute("SELECT 1")
            assert cursor.fetchone() == (1,)
