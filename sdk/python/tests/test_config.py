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
    # Unknown/legacy keys (e.g. profiling) are ignored, not rejected.
    # Bad-token DSN still constructs; registry returns the instance.
    assert lazyobserve.get_client() is lo
    lo.shutdown(timeout=1.0)
