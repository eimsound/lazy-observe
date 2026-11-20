import pytest

import lazyobserve
from lazyobserve.config import (
    Config,
    DeliveryConfig,
    InvalidConfigError,
    normalize_config,
    signal_matrix,
)


def test_signal_matrix_all_default():
    cfg = Config()
    assert signal_matrix(cfg) == (True, True, True)


def test_signal_matrix_explicit():
    cfg = Config(metrics=True)
    assert signal_matrix(cfg) == (True, False, False)
    cfg = Config(metrics=True, traces=True)
    assert signal_matrix(cfg) == (True, False, True)


def test_normalize_defaults():
    cfg = normalize_config(Config(dsn="http://h/p/a@b"))
    assert cfg.sample_rate == 1.0
    assert cfg.export_interval == 5.0
    assert cfg.delivery.error_queue_capacity == 100
    assert cfg.delivery.max_attempts == 3
    assert cfg.delivery.retry_initial == 0.1
    assert cfg.delivery.retry_max == 1.0


def test_profiling_is_off_by_default_with_a_60_second_interval():
    assert Config().profiling is False
    assert Config().profiling_interval == 60.0
    cfg = normalize_config(Config(dsn="http://h/p/a@b"))
    assert cfg.profiling is False
    assert cfg.profiling_interval == 60.0


@pytest.mark.parametrize("value", [0, 0.0, -1, -0.5, float("nan"), float("inf"), float("-inf")])
def test_profiling_interval_that_is_not_positive_and_finite_falls_back_to_default(value):
    cfg = normalize_config(Config(dsn="http://h/p/a@b", profiling_interval=value))
    assert cfg.profiling_interval == 60.0


@pytest.mark.parametrize("value", [5, 5.0, 10.0, 600.0, 0.5])
def test_profiling_interval_is_kept_when_positive(value):
    # Below the 10 s window the windows simply run back to back; no clamping here.
    cfg = normalize_config(Config(dsn="http://h/p/a@b", profiling=True, profiling_interval=value))
    assert cfg.profiling is True
    assert cfg.profiling_interval == value


def test_init_accepts_profiling_keys_from_a_dict():
    fields = lazyobserve.Config.__dataclass_fields__
    assert "profiling" in fields
    assert "profiling_interval" in fields


def test_missing_dsn_invalid():
    with pytest.raises(InvalidConfigError):
        normalize_config(Config(dsn=""))


def test_bad_sample_rate_invalid():
    with pytest.raises(InvalidConfigError):
        normalize_config(Config(dsn="http://h/p/a@b", sample_rate=1.5))


def test_bad_retry_invalid():
    with pytest.raises(InvalidConfigError):
        normalize_config(
            Config(
                dsn="http://h/p/a@b",
                delivery=DeliveryConfig(retry_initial=2.0, retry_max=1.0),
            )
        )


def test_init_degraded_on_bad_dsn():
    lo = lazyobserve.init({"dsn": "garbage", "service_name": "x"})
    assert lo.degraded
    assert lo.err() is not None
    # Degraded instance methods never raise.
    assert lo.capture_exception(ValueError("x")) == ""
    spans = lo.tracer_provider.get_tracer("x").start_span("y")
    spans.end()
    lo.shutdown(timeout=1.0)


def test_init_degraded_ok_on_bad_export():
    lo = lazyobserve.init(
        {"dsn": "http://255.255.255.255:1/p/a@b", "service_name": "x"}
    )
    # Bad-token DSN still constructs; registry returns the instance.
    assert lazyobserve.get_client() is lo
    lo.shutdown(timeout=1.0)


def test_config_repr_hides_dsn_token():
    cfg = Config(dsn="http://127.0.0.1:3100/p/proj@lo_secret_token_999", service_name="svc")
    rendered = repr(cfg)
    assert "lo_secret_token_999" not in rendered
    assert "svc" in rendered


def test_init_preserves_re_raise_on_500_and_nested_delivery_dict():
    lo = lazyobserve.init(
        {
            "dsn": "http://127.0.0.1:1/p/proj@tok",
            "service_name": "cfg-test",
            "traces": True,
            "re_raise_on_500": True,
            "delivery": {
                "error_queue_capacity": 42,
                "max_attempts": 5,
                "retry_initial": 0.2,
                "retry_max": 2.0,
            },
        }
    )
    try:
        assert not lo.degraded, lo.err()
        assert lo.config.re_raise_on_500 is True
        assert isinstance(lo.config.delivery, DeliveryConfig)
        assert lo.config.delivery.error_queue_capacity == 42
        assert lo.config.delivery.max_attempts == 5
        assert lo.config.delivery.retry_initial == 0.2
        assert lo.config.delivery.retry_max == 2.0
    finally:
        lo.shutdown(timeout=1.0)


def test_init_degrades_cleanly_on_invalid_delivery_type():
    lo = lazyobserve.init(
        {
            "dsn": "http://127.0.0.1:1/p/proj@tok",
            "service_name": "bad-delivery",
            "delivery": "not-a-dict",
        }
    )
    assert lo.degraded
    assert isinstance(lo.err(), InvalidConfigError)


def test_import_succeeds_without_unix_resource_module():
    import subprocess
    import sys

    code = (
        "import sys; sys.modules['resource'] = None; "
        "import lazyobserve; from lazyobserve.client import _memory_rss; "
        "assert _memory_rss() == 0; print('ok')"
    )
    proc = subprocess.run(
        [sys.executable, "-c", code],
        check=True,
        capture_output=True,
        text=True,
    )
    assert proc.stdout.strip() == "ok"


def test_build_client_cleans_up_created_providers_on_midway_failure(monkeypatch):
    from lazyobserve import client as client_mod
    from lazyobserve.dsn import parse_dsn

    shutdown_calls = []

    class FakeProvider:
        def __init__(self, name):
            self.name = name

        def get_meter(self, _name):
            raise AssertionError("not expected")

        def get_tracer(self, _name):
            return None

        def shutdown(self):
            shutdown_calls.append(self.name)

    def fake_meter_provider(**kwargs):
        return FakeProvider("meter")

    def fake_metric_reader(*args, **kwargs):
        return object()

    def fake_tracer_provider(**kwargs):
        return FakeProvider("tracer")

    def boom_register(_client):
        raise RuntimeError("runtime metrics registration boom")

    monkeypatch.setattr("opentelemetry.sdk.metrics.MeterProvider", fake_meter_provider)
    monkeypatch.setattr(
        "opentelemetry.sdk.metrics._internal.export.PeriodicExportingMetricReader",
        fake_metric_reader,
    )
    monkeypatch.setattr("opentelemetry.sdk.trace.TracerProvider", fake_tracer_provider)
    monkeypatch.setattr(client_mod, "_register_runtime_metrics", boom_register)

    cfg = normalize_config(
        Config(
            dsn="http://127.0.0.1:1/p/proj@tok",
            service_name="cleanup-svc",
            metrics=True,
            traces=True,
            logs=False,
        )
    )
    with pytest.raises(RuntimeError, match="runtime metrics registration boom"):
        client_mod.build_client(cfg, parse_dsn(cfg.dsn))

    assert shutdown_calls == ["tracer", "meter"]


def test_django_example_settings_do_not_log_plaintext_dsn():
    from pathlib import Path

    settings_path = Path(__file__).resolve().parents[1] / "examples" / "django" / "config" / "settings.py"
    source = settings_path.read_text(encoding="utf-8")
    assert 'extra={"dsn": LO_DSN}' not in source
    assert "dsn=%s" not in source
