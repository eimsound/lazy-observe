import pytest

import lazy_observe
from lazy_observe.config import (
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
    fields = lazy_observe.Config.__dataclass_fields__
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
    lo = lazy_observe.init({"dsn": "garbage", "service_name": "x"})
    assert lo.degraded
    assert lo.err() is not None
    # Degraded instance methods never raise.
    assert lo.capture_exception(ValueError("x")) == ""
    spans = lo.tracer_provider.get_tracer("x").start_span("y")
    spans.end()
    lo.shutdown(timeout=1.0)


def test_init_degraded_ok_on_bad_export():
    lo = lazy_observe.init(
        {"dsn": "http://255.255.255.255:1/p/a@b", "service_name": "x"}
    )
    # Bad-token DSN still constructs; registry returns the instance.
    assert lazy_observe.get_client() is lo
    lo.shutdown(timeout=1.0)


def test_config_repr_hides_dsn_token():
    cfg = Config(dsn="http://127.0.0.1:3100/p/proj@lo_secret_token_999", service_name="svc")
    rendered = repr(cfg)
    assert "lo_secret_token_999" not in rendered
    assert "svc" in rendered


def test_init_preserves_re_raise_on_500_and_nested_delivery_dict():
    lo = lazy_observe.init(
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
    lo = lazy_observe.init(
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

    # Block ``resource`` before importing lazy_observe, then simulate non-Linux
    # (e.g. Windows) when querying ``_memory_rss()`` so neither /proc/self/statm
    # nor ``resource`` is available.
    win_code = (
        "import sys; sys.modules['resource'] = None; "
        "import lazy_observe; from lazy_observe.client import _memory_rss; "
        "sys.platform = 'win32'; "
        "assert _memory_rss() == 0; print('ok-win')"
    )
    win_proc = subprocess.run(
        [sys.executable, "-c", win_code],
        check=True,
        capture_output=True,
        text=True,
    )
    assert win_proc.stdout.strip() == "ok-win"

    # Linux without ``resource`` still imports cleanly and reads live RSS from /proc/self/statm.
    if sys.platform.startswith("linux"):
        linux_code = (
            "import sys; sys.modules['resource'] = None; "
            "import lazy_observe; from lazy_observe.client import _memory_rss; "
            "assert _memory_rss() > 0; print('ok-linux')"
        )
        linux_proc = subprocess.run(
            [sys.executable, "-c", linux_code],
            check=True,
            capture_output=True,
            text=True,
        )
        assert linux_proc.stdout.strip() == "ok-linux"


def test_memory_rss_prefers_proc_statm_on_linux_and_falls_back_to_rusage(tmp_path, monkeypatch):
    from types import SimpleNamespace
    import lazy_observe.client as client_mod

    statm = tmp_path / "statm"
    statm.write_text("5000 1234 400 10 0 800 0\n", encoding="ascii")
    monkeypatch.setattr(client_mod.os, "sysconf", lambda name: 4096)
    assert client_mod._linux_proc_statm_rss(str(statm)) == 1234 * 4096

    # Malformed statm returns None so _memory_rss falls back to getrusage.
    bad_statm = tmp_path / "bad_statm"
    bad_statm.write_text("only-one-field\n", encoding="ascii")
    assert client_mod._linux_proc_statm_rss(str(bad_statm)) is None
    assert client_mod._linux_proc_statm_rss(str(tmp_path / "missing")) is None

    monkeypatch.setattr(client_mod.sys, "platform", "linux")
    monkeypatch.setattr(client_mod, "_linux_proc_statm_rss", lambda: 999_000)
    assert client_mod._memory_rss() == 999_000

    # When /proc/self/statm fails, falls back to resource.getrusage.
    monkeypatch.setattr(client_mod, "_linux_proc_statm_rss", lambda: None)
    fake_resource = SimpleNamespace(
        RUSAGE_SELF=0,
        getrusage=lambda _who: SimpleNamespace(ru_maxrss=256),
    )
    monkeypatch.setitem(client_mod.sys.modules, "resource", fake_resource)
    monkeypatch.setattr(client_mod.platform, "system", lambda: "Linux")
    assert client_mod._memory_rss() == 256 * 1024

    monkeypatch.setattr(client_mod.platform, "system", lambda: "Darwin")
    assert client_mod._memory_rss() == 256


def test_config_deep_copies_tags_against_caller_mutation():
    caller_tags = {"env": "prod", "region": "us-east-1"}
    cfg = Config(dsn="http://127.0.0.1:1/p/proj@tok", service_name="tags-test", tags=caller_tags)
    caller_tags["env"] = "mutated"
    caller_tags["extra"] = "leaked"
    assert cfg.tags == {"env": "prod", "region": "us-east-1"}

    init_tags = {"team": "payments"}
    lo = lazy_observe.init(
        {
            "dsn": "http://127.0.0.1:1/p/proj@tok",
            "service_name": "tags-init",
            "traces": True,
            "tags": init_tags,
        }
    )
    try:
        init_tags["team"] = "mutated"
        assert lo.config.tags == {"team": "payments"}
    finally:
        lo.shutdown(timeout=1.0)


def test_init_warns_once_on_unknown_fields_and_nested_delivery_keys(caplog):
    with caplog.at_level("WARNING", logger="lazy_observe"):
        lo = lazy_observe.init(
            {
                "dsn": "http://127.0.0.1:1/p/proj@tok",
                "service_name": "warn-test",
                "traces": True,
                "serviceName": "typo-top",
                "unknown_flag": True,
                "delivery": {
                    "max_attempts": 4,
                    "bogus_retry": 99,
                },
            }
        )
    try:
        assert not lo.degraded
        assert lo.config.delivery.max_attempts == 4
        unknown_warnings = [
            rec.getMessage()
            for rec in caplog.records
            if "ignoring unknown config field(s)" in rec.getMessage()
        ]
        assert len(unknown_warnings) == 1
        assert "delivery.bogus_retry" in unknown_warnings[0]
        assert "serviceName" in unknown_warnings[0]
        assert "unknown_flag" in unknown_warnings[0]
    finally:
        lo.shutdown(timeout=1.0)


def test_build_client_cleans_up_created_providers_on_midway_failure(monkeypatch):
    from lazy_observe import client as client_mod
    from lazy_observe.dsn import parse_dsn

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
        "opentelemetry.sdk.metrics.export.PeriodicExportingMetricReader",
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


def test_compression_config_validation_and_env_precedence(monkeypatch):
    from lazy_observe.config import normalize_compression, resolve_signal_compression

    assert Config().compression == ""
    assert normalize_compression("") == ""
    assert normalize_compression(None) == ""
    assert normalize_compression("none") == "none"
    assert normalize_compression(" GZIP ") == "gzip"

    with pytest.raises(InvalidConfigError):
        normalize_compression("zstd")
    with pytest.raises(InvalidConfigError):
        normalize_compression(123)

    assert resolve_signal_compression("", "TRACES", {}) == "none"
    assert (
        resolve_signal_compression(
            "",
            "LOGS",
            {"OTEL_EXPORTER_OTLP_COMPRESSION": "gzip"},
        )
        == "gzip"
    )
    assert (
        resolve_signal_compression(
            "",
            "METRICS",
            {
                "OTEL_EXPORTER_OTLP_COMPRESSION": "gzip",
                "OTEL_EXPORTER_OTLP_METRICS_COMPRESSION": "none",
            },
        )
        == "none"
    )
    # Explicit config overrides all env vars, even invalid ones.
    assert (
        resolve_signal_compression(
            "none",
            "TRACES",
            {
                "OTEL_EXPORTER_OTLP_COMPRESSION": "gzip",
                "OTEL_EXPORTER_OTLP_TRACES_COMPRESSION": "invalid-env",
            },
        )
        == "none"
    )
    assert (
        resolve_signal_compression(
            "gzip",
            "TRACES",
            {"OTEL_EXPORTER_OTLP_TRACES_COMPRESSION": "none"},
        )
        == "gzip"
    )

    lo_bad = lazy_observe.init(
        {"dsn": "http://127.0.0.1:1/p/proj@tok", "compression": "br"}
    )
    assert lo_bad.degraded
    assert isinstance(lo_bad.err(), InvalidConfigError)

    monkeypatch.setenv("OTEL_EXPORTER_OTLP_COMPRESSION", "zstd")
    lo_bad_env = lazy_observe.init({"dsn": "http://127.0.0.1:1/p/proj@tok"})
    assert lo_bad_env.degraded
    assert isinstance(lo_bad_env.err(), InvalidConfigError)

    lo_override = lazy_observe.init(
        {"dsn": "http://127.0.0.1:1/p/proj@tok", "compression": "none"}
    )
    try:
        assert not lo_override.degraded, lo_override.err()
        assert lo_override.config.compression == "none"
    finally:
        lo_override.shutdown(timeout=1.0)


def test_compression_wire_encoding_for_all_signals(monkeypatch):
    import gzip
    import threading
    from http.server import BaseHTTPRequestHandler, HTTPServer

    for key in (
        "OTEL_EXPORTER_OTLP_COMPRESSION",
        "OTEL_EXPORTER_OTLP_METRICS_COMPRESSION",
        "OTEL_EXPORTER_OTLP_LOGS_COMPRESSION",
        "OTEL_EXPORTER_OTLP_TRACES_COMPRESSION",
    ):
        monkeypatch.delenv(key, raising=False)

    captured = {}
    lock = threading.Lock()

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):
            length = int(self.headers.get("Content-Length", "0"))
            body = self.rfile.read(length)
            encoding = self.headers.get("Content-Encoding", "")
            with lock:
                captured.setdefault(self.path, []).append((encoding, body))
            self.send_response(200)
            self.end_headers()

        def log_message(self, format, *args):
            return

    server = HTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    endpoint = f"http://127.0.0.1:{server.server_address[1]}"

    try:
        # 1. Default -> uncompressed across metrics, logs, traces
        lo_default = lazy_observe.init(
            dsn=f"{endpoint}/p/proj@lo_tok",
            service_name="py-compress-default",
            tee_to_stderr=False,
        )
        assert not lo_default.degraded, lo_default.err()
        with lo_default.tracer().start_as_current_span("default-span"):
            lo_default.logger_factory().info("default log")
        lo_default.shutdown(timeout=3.0)

        for path in ("/v1/metrics", "/v1/logs", "/v1/traces"):
            assert path in captured
            for encoding, _body in captured[path]:
                assert encoding == ""

        # 2. Explicit gzip -> Content-Encoding: gzip and valid gzip body
        captured.clear()
        lo_gzip = lazy_observe.init(
            {
                "dsn": f"{endpoint}/p/proj@lo_tok",
                "service_name": "py-compress-gzip",
                "compression": "gzip",
                "tee_to_stderr": False,
            }
        )
        assert not lo_gzip.degraded, lo_gzip.err()
        with lo_gzip.tracer().start_as_current_span("gzip-span"):
            lo_gzip.logger_factory().info("gzip log")
        lo_gzip.shutdown(timeout=3.0)

        for path in ("/v1/metrics", "/v1/logs", "/v1/traces"):
            assert path in captured
            for encoding, body in captured[path]:
                assert encoding == "gzip"
                assert len(gzip.decompress(body)) > 0

        # 3. Env precedence + explicit "none" override
        captured.clear()
        monkeypatch.setenv("OTEL_EXPORTER_OTLP_COMPRESSION", "gzip")
        monkeypatch.setenv("OTEL_EXPORTER_OTLP_METRICS_COMPRESSION", "none")
        lo_env = lazy_observe.init(
            {
                "dsn": f"{endpoint}/p/proj@lo_tok",
                "service_name": "py-compress-env",
                "tee_to_stderr": False,
            }
        )
        assert not lo_env.degraded, lo_env.err()
        with lo_env.tracer().start_as_current_span("env-span"):
            lo_env.logger_factory().info("env log")
        lo_env.shutdown(timeout=3.0)

        assert all(enc == "" for enc, _ in captured["/v1/metrics"])
        assert all(enc == "gzip" for enc, _ in captured["/v1/logs"])
        assert all(enc == "gzip" for enc, _ in captured["/v1/traces"])

        captured.clear()
        monkeypatch.setenv("OTEL_EXPORTER_OTLP_METRICS_COMPRESSION", "gzip")
        lo_none = lazy_observe.init(
            {
                "dsn": f"{endpoint}/p/proj@lo_tok",
                "service_name": "py-compress-override",
                "compression": "none",
                "tee_to_stderr": False,
            }
        )
        assert not lo_none.degraded, lo_none.err()
        with lo_none.tracer().start_as_current_span("none-span"):
            lo_none.logger_factory().info("none log")
        lo_none.shutdown(timeout=3.0)

        for path in ("/v1/metrics", "/v1/logs", "/v1/traces"):
            assert path in captured
            for encoding, _ in captured[path]:
                assert encoding == ""
    finally:
        server.shutdown()
        server.server_close()
