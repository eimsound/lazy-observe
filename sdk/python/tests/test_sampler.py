"""Tests for the CPython 3.15 in-process CPU sampler.

Most tests drive ``lazyobserve._sampler`` through a fake ``profiling.sampling``
package, so they run on 3.9-3.14 too. The fake mirrors what 3.15.2 really hands
out: ``sample()`` is called as ``sample(pid, collector, *, duration_sec,
all_threads, mode)``, stacks arrive as ``InterpreterInfo -> ThreadInfo ->
frame_info`` with frame tuples ``(filename, location, funcname, opcode)``,
``location`` is a ``LocationInfo`` namedtuple (``location[0]`` is the line),
``funcname`` is the qualname, and ``thread_id`` is the OS thread id
(``threading.get_native_id()``), not ``threading.get_ident()``.

The ``needs_real_sampling`` tests sample the real interpreter and only run on
Linux with a free-threaded CPython 3.15+ whose GIL is off; on a 3.15 GIL build a
real test asserts the sampler reports itself unavailable instead.
"""

from __future__ import annotations

import os
import sys
import sysconfig
import threading
import time
import types
from collections import namedtuple

import pytest

from lazyobserve import _sampler

# 3.15 hands out ``_remote_debugging.LocationInfo`` namedtuples.
Location = namedtuple("Location", "lineno end_lineno col_offset end_col_offset")


class _ThreadInfo:
    def __init__(self, thread_id, frames, status=0):
        self.thread_id = thread_id
        self.frame_info = frames
        self.status = status


class _InterpreterInfo:
    def __init__(self, threads):
        self.threads = threads


def _frame(funcname, filename="app.py", lineno=1):
    """A 3.15 frame tuple: (filename, location, funcname, opcode)."""
    return (filename, Location(lineno, lineno, 0, 0), funcname, None)


def _stacks(*threads):
    """Build the payload ``sample()`` hands to ``collector.collect``."""
    return [_InterpreterInfo([_ThreadInfo(thread_id, frames) for thread_id, frames in threads])]


class FakeSampling:
    """Fake ``profiling.sampling`` package: sample function plus constants."""

    PROFILING_MODE_WALL = 0
    PROFILING_MODE_CPU = 1

    def __init__(self, *, stacks=None, error=None, timestamps=1, failed=0, printed=None):
        self.stacks = stacks
        self.error = error
        self.timestamps = timestamps
        # Failed attempts per window, reported like ``SampleProfiler.sample`` does
        # for RuntimeError/OSError: ``collector.collect_failed_sample()``.
        self.failed = failed
        self.printed = printed
        self.calls = []
        self.module = None

    def sample(self, pid, collector, *, duration_sec=None, all_threads=False, mode=0):
        self.calls.append(
            {
                "pid": pid,
                "collector": collector,
                "duration_sec": duration_sec,
                "all_threads": all_threads,
                "mode": mode,
            }
        )
        if self.printed is not None:
            # Like sample.py: the module-level ``print`` name, not ``builtins``.
            self.module.print(self.printed)
        if self.error is not None:
            raise self.error
        if self.stacks is not None:
            collector.collect(self.stacks, timestamps_us=[0] * self.timestamps)
        for _ in range(self.failed):
            collector.collect_failed_sample()
        return collector

    def sample_module(self):
        module = types.ModuleType("profiling.sampling.sample")
        module.sample = self.sample
        self.module = module
        return module

    def constants_module(self):
        module = types.ModuleType("profiling.sampling.constants")
        module.PROFILING_MODE_WALL = self.PROFILING_MODE_WALL
        module.PROFILING_MODE_CPU = self.PROFILING_MODE_CPU
        return module

    def importer(self):
        modules = {
            "profiling.sampling.sample": self.sample_module(),
            "profiling.sampling.constants": self.constants_module(),
        }

        def import_module(name):
            try:
                return modules[name]
            except KeyError:
                raise ModuleNotFoundError(name) from None

        return import_module

    def probe(self, *, version_info=(3, 15, 0), trial_seconds=0.01, **runtime):
        # Healthy runtime by default (Linux, free-threaded build, GIL off), so
        # these tests do not depend on the interpreter running the suite.
        runtime = _runtime(**runtime)
        return _sampler.probe_sampler(
            importer=self.importer(),
            version_info=version_info,
            trial_seconds=trial_seconds,
            **runtime,
        )


def _runtime(*, platform="linux", gil_build=True, gil_enabled=False):
    """Fake ``sys``/``sysconfig`` modules for the three availability gates.

    ``gil_enabled=None`` removes ``sys._is_gil_enabled`` (a pre-3.13 interpreter).
    """
    fake_sys = types.SimpleNamespace(platform=platform)
    if gil_enabled is not None:
        fake_sys._is_gil_enabled = lambda: gil_enabled
    fake_sysconfig = types.SimpleNamespace(
        get_config_var=lambda name: (1 if gil_build else 0) if name == "Py_GIL_DISABLED" else None
    )
    return {"sys_module": fake_sys, "sysconfig_module": fake_sysconfig}


def _forbidden_importer(name):
    raise AssertionError(f"imported {name} below 3.15")


def _real_sampling_possible():
    """True on Linux + a free-threaded 3.15+ build whose GIL is really off."""
    return (
        sys.version_info >= (3, 15)
        and sys.platform == "linux"
        and bool(sysconfig.get_config_var("Py_GIL_DISABLED"))
        and getattr(sys, "_is_gil_enabled", lambda: True)() is False
    )


def _sampler_for(fake, **kwargs):
    probe = fake.probe()
    assert probe.available, probe
    fake.calls.clear()  # drop the probe's trial call, keep window calls
    return _sampler.Sampler(probe, **kwargs)


def _warn_recorder():
    calls = []

    def warn(reason, err, count):
        calls.append((reason, err, count))

    return calls, warn


def test_defaults_match_the_decision():
    assert _sampler.SAMPLING_RATE_HZ == 100
    assert _sampler.SAMPLE_PERIOD_NS == 10_000_000
    assert _sampler.SAMPLE_INTERVAL_USEC == 10_000
    # Go SDK cadence: a 10 s window every 60 s (ADR 0014, revised).
    assert _sampler.WINDOW_SECONDS == 10.0
    assert _sampler.WINDOW_INTERVAL_SECONDS == 60.0
    assert _sampler.MAX_CONSECUTIVE_FAILURES == 3
    # Owner decision (ADR 0014, second revision): stop once more than half the ticks fail.
    assert _sampler.FAILURE_RATE_THRESHOLD == 0.5
    assert _sampler.MIN_FAILURE_RATE_ATTEMPTS == 100


# --- availability gates (before any trial sample) --------------------------


@pytest.mark.parametrize(
    ("runtime", "reason"),
    [
        ({"platform": "darwin"}, _sampler.REASON_PLATFORM),
        ({"platform": "win32"}, _sampler.REASON_PLATFORM),
        ({"gil_build": False}, _sampler.REASON_GIL_BUILD),
        ({"gil_enabled": True}, _sampler.REASON_GIL_ENABLED),
        ({"gil_enabled": None}, _sampler.REASON_GIL_ENABLED),  # no sys._is_gil_enabled
    ],
    ids=["macos", "windows", "gil-build", "gil-reenabled", "no-is-gil-enabled"],
)
def test_probe_refuses_before_importing_or_trial_sampling(runtime, reason):
    fake = FakeSampling(stacks=_stacks((1, [_frame("handler")])))
    imported = []

    def importer(name):
        imported.append(name)
        return fake.importer()(name)

    probe = _sampler.probe_sampler(
        importer=importer, version_info=(3, 15, 0), trial_seconds=0.01, **_runtime(**runtime)
    )

    assert probe.available is False
    assert probe.reason == reason
    assert probe.detail
    # A GIL build "samples" without error and returns nothing, so the gates
    # must run before the trial sample and before the private import.
    assert fake.calls == []
    assert imported == []
    assert _sampler.Sampler(probe).collect_window(1.0) is None
    assert fake.calls == []


def test_probe_gil_state_that_raises_counts_as_enabled():
    runtime = _runtime()

    def broken():
        raise RuntimeError("no gil state")

    runtime["sys_module"]._is_gil_enabled = broken

    probe = _sampler.probe_sampler(importer=_forbidden_importer, version_info=(3, 15, 0), **runtime)

    assert probe.available is False
    assert probe.reason == _sampler.REASON_GIL_ENABLED


def test_probe_below_315_reports_the_version_first():
    probe = _sampler.probe_sampler(
        importer=_forbidden_importer, version_info=(3, 14, 0), **_runtime(platform="darwin")
    )

    assert probe.reason == _sampler.REASON_PYTHON_TOO_OLD


def test_recheck_gil_stops_the_sampler_when_the_gil_comes_back():
    fake = FakeSampling(stacks=_stacks((1, [_frame("handler")])))
    gil = {"enabled": False}
    sampler = _sampler_for(fake, gil_enabled=lambda: gil["enabled"])

    assert sampler.recheck_gil() is True
    assert sampler.collect_window(1.0) is not None
    assert len(fake.calls) == 1

    gil["enabled"] = True  # e.g. a C extension that needs the GIL was imported
    assert sampler.recheck_gil() is False
    assert sampler.available() is False
    assert sampler.reason() == _sampler.REASON_GIL_ENABLED
    assert sampler.collect_window(1.0) is None
    assert len(fake.calls) == 1, "must not sample once the GIL is back"

    gil["enabled"] = False  # stopped for good, not flapping
    assert sampler.recheck_gil() is False


def test_recheck_gil_on_an_unavailable_sampler_is_false():
    probe = FakeSampling().probe(platform="darwin")
    sampler = _sampler.Sampler(probe, gil_enabled=lambda: False)

    assert sampler.recheck_gil() is False
    assert sampler.reason() == _sampler.REASON_PLATFORM


def test_gil_enabled_helper_reads_the_runtime():
    assert _sampler.gil_enabled(_runtime(gil_enabled=False)["sys_module"]) is False
    assert _sampler.gil_enabled(_runtime(gil_enabled=True)["sys_module"]) is True
    assert _sampler.gil_enabled(_runtime(gil_enabled=None)["sys_module"]) is True


# --- feature probing -------------------------------------------------------


def test_probe_below_315_does_not_import_private_modules():
    probe = _sampler.probe_sampler(importer=_forbidden_importer, version_info=(3, 14, 0))

    assert probe.available is False
    assert probe.reason == _sampler.REASON_PYTHON_TOO_OLD


@pytest.mark.skipif(sys.version_info >= (3, 15), reason="asserts the pre-3.15 behaviour")
def test_probe_on_a_real_pre_315_interpreter_imports_nothing():
    before = {name.split(".")[0] for name in sys.modules}

    probe = _sampler.probe_sampler()

    assert probe.available is False
    assert probe.reason == _sampler.REASON_PYTHON_TOO_OLD
    imported = {name.split(".")[0] for name in sys.modules} - before
    assert "profiling" not in imported


def test_probe_reports_import_failure():
    def import_module(name):
        raise ModuleNotFoundError(name)

    probe = _sampler.probe_sampler(importer=import_module, version_info=(3, 15, 0), **_runtime())

    assert probe.available is False
    assert probe.reason == _sampler.REASON_IMPORT_FAILED


def test_probe_reports_signature_mismatch_without_trial_sampling():
    fake = FakeSampling()

    def importer(name):
        module = fake.importer()(name)
        if name == "profiling.sampling.sample":

            def sample(pid, collector):
                return collector

            module.sample = sample
        return module

    probe = _sampler.probe_sampler(importer=importer, version_info=(3, 15, 0), **_runtime())

    assert probe.available is False
    assert probe.reason == _sampler.REASON_SIGNATURE_MISMATCH
    assert "mode" in probe.detail
    assert fake.calls == []


def test_probe_reports_missing_cpu_mode_constant():
    fake = FakeSampling()

    def importer(name):
        module = fake.importer()(name)
        if name == "profiling.sampling.constants":
            del module.PROFILING_MODE_CPU
        return module

    probe = _sampler.probe_sampler(importer=importer, version_info=(3, 15, 0), **_runtime())

    assert probe.available is False
    assert probe.reason == _sampler.REASON_MODE_MISSING


def test_probe_reports_trial_failure_without_leaking_the_exception():
    fake = FakeSampling(error=SystemExit("permission denied"))

    probe = fake.probe()

    assert probe.available is False
    assert probe.reason == _sampler.REASON_TRIAL_FAILED
    assert "permission denied" in probe.detail
    assert len(fake.calls) == 1
    assert fake.calls[0]["all_threads"] is True
    assert fake.calls[0]["mode"] == FakeSampling.PROFILING_MODE_CPU
    assert fake.calls[0]["pid"] == os.getpid()


def test_trial_sample_stats_do_not_reach_stdout(capsys):
    # ``SampleProfiler.sample`` prints "Captured N samples..." to stdout; the
    # probe runs it through the real silencing hook, so nothing leaks.
    fake = FakeSampling(printed="Captured 10 samples in 0.10 seconds")

    probe = fake.probe()

    assert probe.available is True
    assert len(fake.calls) == 1, "trial sample must have run"
    assert capsys.readouterr().out == ""
    # The hook replaces the module-level ``print`` of profiling.sampling.sample.
    probe.module.print("Captured 10 samples in 0.10 seconds")
    assert capsys.readouterr().out == ""


# --- window collection -----------------------------------------------------


def test_collect_window_samples_the_current_process_with_cpu_mode():
    fake = FakeSampling(stacks=_stacks((1, [_frame("handler")])))

    sampler = _sampler_for(fake)
    window = sampler.collect_window(60.0)

    assert window is not None
    assert len(fake.calls) == 1
    call = fake.calls[0]
    assert call["pid"] == os.getpid()
    assert call["all_threads"] is True
    assert call["mode"] == FakeSampling.PROFILING_MODE_CPU
    assert call["duration_sec"] == 60.0
    assert call["collector"].sample_interval_usec == 10_000
    assert call["collector"].aggregating is True


def test_collect_window_aggregates_leaf_first_tuples_weighted_by_timestamps():
    frames = [_frame("Inner.handle", "app.py", 7), _frame("dispatch", "app.py", 20)]
    fake = FakeSampling(stacks=_stacks((1, frames)), timestamps=4)

    window = _sampler_for(fake).collect_window(1.0)

    assert window is not None
    assert list(window.counts.items()) == [
        ((("Inner.handle", "app.py", 7), ("dispatch", "app.py", 20)), 4)
    ]
    assert window.samples == 4


def test_collect_window_skips_frames_without_a_function_name():
    frames = [_frame(""), _frame("kept", "app.py", 3)]
    fake = FakeSampling(stacks=_stacks((1, frames)))

    window = _sampler_for(fake).collect_window(1.0)

    assert window is not None
    assert list(window.counts) == [(("kept", "app.py", 3),)]


def test_collect_window_accepts_object_frames():
    class _Frame:
        def __init__(self, filename, location, funcname):
            self.filename = filename
            self.location = location
            self.funcname = funcname

    frames = [_Frame("app.py", 42, "Obj.method"), _Frame("app.py", None, "caller")]
    fake = FakeSampling(stacks=_stacks((1, frames)))

    window = _sampler_for(fake).collect_window(1.0)

    assert window is not None
    assert list(window.counts) == [(("Obj.method", "app.py", 42), ("caller", "app.py", 0))]


def test_collect_window_excludes_the_sampling_thread_by_native_id():
    native_id = threading.get_native_id()
    self_frames = [_frame("_sampler.collect_window", "lazyobserve/_sampler.py", 200)]
    other_frames = [_frame("view", "app.py", 11)]
    fake = FakeSampling(stacks=_stacks((native_id, self_frames), (native_id + 1, other_frames)))

    window = _sampler_for(fake).collect_window(1.0)

    assert window is not None
    assert list(window.counts) == [(("view", "app.py", 11),)]
    assert native_id != threading.get_ident()


def test_collect_window_reports_timestamps_and_duration():
    fake = FakeSampling(stacks=_stacks((1, [_frame("handler")])))

    window = _sampler_for(fake).collect_window(0.5)

    assert window is not None
    assert window.start_ns <= window.end_ns
    assert window.end_ns - window.start_ns == window.duration_ns
    assert window.duration_ns >= 0


def test_unavailable_sampler_returns_no_window_and_never_samples():
    fake = FakeSampling(stacks=_stacks((1, [_frame("handler")])))
    probe = fake.probe(version_info=(3, 14, 0))
    sampler = _sampler.Sampler(probe)

    assert sampler.available() is False
    assert sampler.reason() == _sampler.REASON_PYTHON_TOO_OLD
    assert sampler.collect_window(1.0) is None
    assert fake.calls == []


def test_create_wires_the_probe(monkeypatch):
    fake = FakeSampling()
    real_probe = _sampler.probe_sampler
    monkeypatch.setattr(
        _sampler,
        "probe_sampler",
        lambda **kwargs: real_probe(
            importer=fake.importer(), version_info=(3, 15, 0), **_runtime()
        ),
    )

    assert _sampler.Sampler.create().available() is True
    assert _sampler.Sampler.create().reason() is None


def test_collect_window_rejects_non_positive_duration():
    sampler = _sampler_for(FakeSampling())

    with pytest.raises(ValueError):
        sampler.collect_window(0)


# --- failure handling ------------------------------------------------------


def test_aggregating_collector_is_running_until_told_to_stop():
    # ``SampleProfiler.sample`` checks ``collector.running`` once per tick and
    # breaks (then flushes what it aggregated) when it is False.
    collector = _sampler._AggregatingCollector(10_000)
    assert collector.running is True


def test_stop_ends_a_window_in_flight_and_keeps_what_was_aggregated():
    started = threading.Event()
    seen = {}

    def sample(pid, collector, *, duration_sec=None, all_threads=False, mode=0):
        seen["collector"] = collector
        started.set()
        deadline = time.monotonic() + 5
        # What 3.15.2's loop does: ``hasattr(collector, "running") and not collector.running``.
        while collector.running and time.monotonic() < deadline:
            time.sleep(0.001)
        collector.collect(_stacks((1, [_frame("work")])), timestamps_us=[0] * 7)

    probe = _sampler.SamplerProbe(True, mode=1, sample=sample)
    sampler = _sampler.Sampler(probe)
    result = {}
    worker = threading.Thread(target=lambda: result.update(window=sampler.collect_window(60.0)))
    worker.start()
    assert started.wait(5)

    began = time.monotonic()
    sampler.stop()
    worker.join(5)

    assert not worker.is_alive()
    assert time.monotonic() - began < 2.0  # nowhere near the 60 s window
    assert seen["collector"].running is False
    window = result["window"]
    assert window is not None
    assert window.counts == {(("work", "app.py", 1),): 7}
    assert window.duration_ns < 5_000_000_000


def test_stop_before_a_window_starts_samples_nothing():
    fake = FakeSampling(stacks=_stacks((1, [_frame("work")])))
    sampler = _sampler_for(fake)
    sampler.stop()
    assert sampler.collect_window(10.0) is None
    assert fake.calls == []
    assert sampler.last_failed_ticks() == 0
    # Stopping is a request from shutdown, not a failure of the sampler.
    assert not sampler.disabled()
    assert sampler.consecutive_failures() == 0


def test_stop_is_idempotent_and_safe_on_an_unavailable_sampler():
    sampler = _sampler.Sampler(_sampler.SamplerProbe(False, reason=_sampler.REASON_GIL_BUILD))
    sampler.stop()
    sampler.stop()
    assert sampler.collect_window(1.0) is None


def test_collect_window_swallows_sampling_errors():
    fake = FakeSampling()
    sampler = _sampler_for(fake)
    fake.error = RuntimeError("unwinder exploded")

    window = sampler.collect_window(1.0)

    assert window is None
    assert sampler.available() is True
    assert sampler.consecutive_failures() == 1


def test_three_consecutive_failures_disable_the_sampler_and_warn():
    fake = FakeSampling()
    calls, warn = _warn_recorder()
    sampler = _sampler_for(fake, warn=warn)
    fake.error = RuntimeError("unwinder exploded")

    windows = [sampler.collect_window(1.0) for _ in range(_sampler.MAX_CONSECUTIVE_FAILURES)]

    assert windows == [None, None, None]
    assert sampler.available() is False
    assert sampler.disabled() is True
    assert sampler.reason() == _sampler.REASON_DISABLED
    assert sampler.consecutive_failures() == 3
    assert len(fake.calls) == 3
    assert [count for _, _, count in calls] == [1, 2, 3]
    assert "disabled" in calls[-1][0]
    assert isinstance(calls[-1][1], RuntimeError)

    assert sampler.collect_window(1.0) is None
    assert len(fake.calls) == 3, "a disabled sampler must stop calling sample()"


def test_sampler_has_no_retry_backoff():
    # A disabled sampler stays disabled for the life of the process; backoff is the
    # upload queue's business (DeliveryQueue), not the sampler's.
    assert not hasattr(_sampler.Sampler, "backoff_seconds")
    assert not hasattr(_sampler, "RETRY_BACKOFF_INITIAL")


def _disable_by_consecutive_failures(fake, sampler):
    fake.error = RuntimeError("unwinder exploded")
    for _ in range(_sampler.MAX_CONSECUTIVE_FAILURES):
        sampler.collect_window(1.0)
    fake.error = None


def _disable_by_failure_rate(fake, sampler):
    fake.stacks = _stacks((1, [_frame("handler")]))
    fake.timestamps, fake.failed = 40, 60
    sampler.collect_window(1.0)
    fake.timestamps, fake.failed = 1, 0


def _disable_by_gil(fake, sampler):
    assert sampler.recheck_gil() is False


@pytest.mark.parametrize(
    ("disable", "reason"),
    [
        (_disable_by_consecutive_failures, _sampler.REASON_DISABLED),
        (_disable_by_failure_rate, _sampler.REASON_FAILURE_RATE),
        (_disable_by_gil, _sampler.REASON_GIL_ENABLED),
    ],
    ids=["three-failed-windows", "tick-failure-rate", "gil-back"],
)
def test_a_disabled_sampler_never_opens_another_window(disable, reason):
    fake = FakeSampling(stacks=_stacks((1, [_frame("handler")])))
    sampler = _sampler_for(fake, gil_enabled=lambda: disable is _disable_by_gil)

    disable(fake, sampler)
    assert sampler.disabled() is True
    assert sampler.reason() == reason
    calls_when_disabled = len(fake.calls)

    # Whatever happens afterwards (a healthy fake, plenty of time), it stays off.
    for _ in range(5):
        assert sampler.collect_window(1.0) is None
        assert sampler.recheck_gil() is False
    assert len(fake.calls) == calls_when_disabled, "no sample() call after disabling"
    assert sampler.available() is False
    assert sampler.reason() == reason
    assert sampler.last_failed_ticks() == 0


def test_intermittent_sample_failures_disable_by_failure_rate_not_by_streak():
    # Every window completes and 51% of its ticks fail, so the consecutive-window
    # counter never gets past 0: only the tick rate rule can catch this.
    calls, warn = _warn_recorder()
    fake = FakeSampling(stacks=_stacks((1, [_frame("handler")])), timestamps=49, failed=51)
    sampler = _sampler_for(fake, warn=warn)

    window = sampler.collect_window(1.0)

    assert window is None, "a window over the threshold is discarded"
    assert sampler.consecutive_failures() == 0
    assert sampler.last_failure_rate() == pytest.approx(0.51)
    assert sampler.available() is False
    assert sampler.disabled() is True
    assert sampler.reason() == _sampler.REASON_FAILURE_RATE
    assert "failure rate" in sampler.detail()
    assert len(calls) == 1 and "failure rate" in calls[0][0] and "disabled" in calls[0][0]
    assert "51 of 100" in calls[0][0]
    assert sampler.collect_window(1.0) is None
    assert len(fake.calls) == 1, "a disabled sampler must stop calling sample()"


def test_49_percent_failed_ticks_do_not_disable():
    fake = FakeSampling(stacks=_stacks((1, [_frame("handler")])), timestamps=51, failed=49)
    sampler = _sampler_for(fake)

    window = sampler.collect_window(1.0)

    assert window is not None
    assert window.failure_rate == pytest.approx(0.49)
    assert sampler.available() is True
    assert sampler.disabled() is False


def test_failed_ticks_are_handed_to_the_caller_even_when_the_window_is_discarded():
    # S3 records them as record_telemetry_drop("profile", "sample_failed", n).
    ok = FakeSampling(stacks=_stacks((1, [_frame("handler")])), timestamps=90, failed=10)
    sampler = _sampler_for(ok)
    window = sampler.collect_window(1.0)
    assert window is not None and window.failed_samples == 10
    assert sampler.last_failed_ticks() == 10

    over = FakeSampling(stacks=_stacks((1, [_frame("handler")])), timestamps=49, failed=51)
    sampler = _sampler_for(over)
    assert sampler.collect_window(1.0) is None
    assert sampler.last_failed_ticks() == 51

    raising = FakeSampling()
    sampler = _sampler_for(raising)
    raising.error = RuntimeError("boom")
    assert sampler.collect_window(1.0) is None
    assert sampler.last_failed_ticks() == 0


def test_a_window_where_every_tick_failed_is_not_returned_for_upload():
    # Below the minimum attempts it is not judged for disabling, but there is
    # nothing to upload either: None, with the failed ticks still reported.
    fake = FakeSampling(failed=7)
    sampler = _sampler_for(fake)

    assert sampler.collect_window(1.0) is None
    assert sampler.last_failed_ticks() == 7
    assert sampler.last_failure_rate() == 1.0
    assert sampler.available() is True
    assert sampler.consecutive_failures() == 0

    # With enough attempts the same window also disables the sampler.
    fake = FakeSampling(failed=_sampler.MIN_FAILURE_RATE_ATTEMPTS)
    sampler = _sampler_for(fake)
    assert sampler.collect_window(1.0) is None
    assert sampler.reason() == _sampler.REASON_FAILURE_RATE
    assert sampler.last_failed_ticks() == _sampler.MIN_FAILURE_RATE_ATTEMPTS


def test_failure_rate_is_exposed_per_window_and_threshold_is_not_exceeded_at_equality():
    fake = FakeSampling(stacks=_stacks((1, [_frame("handler")])), timestamps=50, failed=50)
    sampler = _sampler_for(fake)

    window = sampler.collect_window(1.0)

    assert window is not None
    assert window.attempts == 100
    assert window.failed_samples == 50
    assert window.failure_rate == pytest.approx(0.5)
    assert sampler.last_failure_rate() == pytest.approx(0.5)
    assert sampler.failure_rate_threshold == 0.5
    assert sampler.available() is True


def test_failure_rate_counts_empty_and_self_stacks_as_successful_attempts():
    # Failures are measured against every attempt the profiler made, not only the
    # ones that left a stack behind: an idle process must not look "mostly failed".
    native_id = threading.get_native_id()
    fake = FakeSampling(stacks=_stacks((native_id, [_frame("self")])), timestamps=50, failed=50)
    sampler = _sampler_for(fake)

    window = sampler.collect_window(1.0)

    assert window is not None
    assert window.samples == 0
    assert window.attempts == 100
    assert window.failure_rate == pytest.approx(0.5)
    assert sampler.available() is True


def test_window_without_attempts_has_a_zero_failure_rate():
    sampler = _sampler_for(FakeSampling())

    window = sampler.collect_window(1.0)

    assert window is not None
    assert window.attempts == 0
    assert window.failure_rate == 0.0
    assert sampler.last_failure_rate() == 0.0


def test_failure_rate_threshold_is_a_parameter():
    fake = FakeSampling(stacks=_stacks((1, [_frame("handler")])), timestamps=70, failed=30)

    default = _sampler_for(fake)
    assert default.collect_window(1.0) is not None
    assert default.available() is True

    strict = _sampler_for(fake, failure_rate_threshold=0.1)
    assert strict.collect_window(1.0) is None
    assert strict.reason() == _sampler.REASON_FAILURE_RATE

    with pytest.raises(ValueError):
        _sampler.Sampler(fake.probe(), failure_rate_threshold=1.5)


def test_a_short_window_is_not_judged_by_its_failure_rate():
    # A window cut short (e.g. by shutdown) with a handful of attempts: three
    # failures out of four is 75%, which says nothing yet. The rate is still exposed.
    fake = FakeSampling(stacks=_stacks((1, [_frame("handler")])), timestamps=1, failed=3)
    sampler = _sampler_for(fake)

    window = sampler.collect_window(1.0)

    assert window is not None
    assert window.attempts == 4
    assert window.failure_rate == pytest.approx(0.75)
    assert sampler.last_failure_rate() == pytest.approx(0.75)
    assert sampler.available() is True

    strict = _sampler_for(fake, min_failure_rate_attempts=0)
    assert strict.collect_window(1.0) is None
    assert strict.reason() == _sampler.REASON_FAILURE_RATE


def test_failure_rate_is_judged_from_the_minimum_attempts_on():
    minimum = _sampler.MIN_FAILURE_RATE_ATTEMPTS
    failed = minimum * 3 // 4  # 75% > 50%
    fake = FakeSampling(
        stacks=_stacks((1, [_frame("handler")])), timestamps=minimum - failed, failed=failed
    )
    sampler = _sampler_for(fake)

    assert sampler.collect_window(1.0) is None
    assert sampler.reason() == _sampler.REASON_FAILURE_RATE


def test_a_window_under_the_threshold_does_not_disable_later_windows():
    fake = FakeSampling(stacks=_stacks((1, [_frame("handler")])), timestamps=95, failed=5)
    sampler = _sampler_for(fake)

    for _ in range(5):
        assert sampler.collect_window(1.0) is not None

    assert sampler.available() is True
    assert sampler.last_failure_rate() == pytest.approx(0.05)


def test_empty_window_is_not_a_failure():
    fake = FakeSampling(stacks=_stacks((1, [])))

    sampler = _sampler_for(fake)
    window = sampler.collect_window(1.0)

    assert window is not None
    assert window.counts == {}
    assert window.samples == 0
    assert sampler.consecutive_failures() == 0
    assert sampler.available() is True


def test_a_successful_window_resets_the_failure_count():
    calls, warn = _warn_recorder()
    fake = FakeSampling()
    sampler = _sampler_for(fake, warn=warn)
    fake.error = RuntimeError("boom")
    sampler.collect_window(1.0)
    sampler.collect_window(1.0)
    assert sampler.consecutive_failures() == 2

    fake.error = None
    fake.stacks = _stacks((1, [_frame("handler")]))
    window = sampler.collect_window(1.0)
    assert window is not None
    # A window only counts as a success once it is encoded, too.
    assert sampler.consecutive_failures() == 2
    assert sampler.encode_window(window) is not None
    assert sampler.consecutive_failures() == 0

    fake.error = RuntimeError("boom")
    sampler.collect_window(1.0)
    sampler.collect_window(1.0)
    assert sampler.consecutive_failures() == 2
    assert sampler.available() is True


def test_encoder_errors_are_counted_and_never_escape():
    def encoder(*args, **kwargs):
        raise TypeError("frame must be a (function, filename, line) tuple")

    calls, warn = _warn_recorder()
    fake = FakeSampling(stacks=_stacks((1, [_frame("handler")])))
    sampler = _sampler_for(fake, warn=warn, encoder=encoder)

    window = sampler.collect_window(1.0)
    assert window is not None
    assert sampler.encode_window(window) is None
    assert sampler.consecutive_failures() == 1
    assert calls and "encode" in calls[-1][0]

    for _ in range(_sampler.MAX_CONSECUTIVE_FAILURES):
        sampler.encode_window(window)
    assert sampler.available() is False


def test_a_successful_collect_does_not_clear_an_encode_failure_streak():
    def encoder(*args, **kwargs):
        raise ValueError("bad frame")

    fake = FakeSampling(stacks=_stacks((1, [_frame("handler")])))
    sampler = _sampler_for(fake, encoder=encoder)

    for expected in range(1, _sampler.MAX_CONSECUTIVE_FAILURES):
        window = sampler.collect_window(1.0)  # collect succeeds...
        assert window is not None
        assert sampler.consecutive_failures() == expected - 1, "collecting must not reset it"
        assert sampler.encode_window(window) is None  # ...encode fails
        assert sampler.consecutive_failures() == expected, "encode failure counts once per window"
    assert sampler.available() is True


def test_encoder_failures_disable_across_windows():
    # The real loop collects, then encodes, every window. A successful collect must
    # not wipe out the previous window's encode failure, or a broken encoder would
    # warn forever instead of reaching the consecutive-failure limit.
    def encoder(*args, **kwargs):
        raise ValueError("bad frame")

    fake = FakeSampling(stacks=_stacks((1, [_frame("handler")])))
    sampler = _sampler_for(fake, encoder=encoder)

    for _ in range(_sampler.MAX_CONSECUTIVE_FAILURES):
        window = sampler.collect_window(1.0)
        assert window is not None
        assert sampler.encode_window(window) is None

    assert sampler.available() is False
    assert sampler.reason() == _sampler.REASON_DISABLED
    assert len(fake.calls) == _sampler.MAX_CONSECUTIVE_FAILURES


def test_encode_window_produces_gzip_pprof_bytes():
    fake = FakeSampling(stacks=_stacks((1, [_frame("handler", "app.py", 3)])))
    sampler = _sampler_for(fake)

    window = sampler.collect_window(1.0)
    assert window is not None
    data = sampler.encode_window(window)

    assert isinstance(data, bytes) and data.startswith(b"\x1f\x8b")


def test_encode_window_of_none_is_none():
    sampler = _sampler_for(FakeSampling())

    assert sampler.encode_window(None) is None


# --- real 3.15 interpreter -------------------------------------------------


needs_real_sampling = pytest.mark.skipif(
    not _real_sampling_possible(),
    reason="needs Linux + a free-threaded CPython 3.15+ whose GIL is disabled at runtime",
)


@pytest.mark.skipif(
    sys.version_info < (3, 15) or bool(sysconfig.get_config_var("Py_GIL_DISABLED")),
    reason="asserts the 3.15 GIL-build behaviour",
)
def test_real_315_gil_build_is_unavailable():
    probe = _sampler.probe_sampler()

    assert probe.available is False
    assert probe.reason == _sampler.REASON_GIL_BUILD
    sampler = _sampler.Sampler.create()
    assert sampler.available() is False
    assert sampler.collect_window(0.2) is None


@pytest.mark.skipif(
    not bool(sysconfig.get_config_var("Py_GIL_DISABLED"))
    or sys.version_info < (3, 15)
    or getattr(sys, "_is_gil_enabled", lambda: False)() is not True,
    reason="asserts the free-threaded build with the GIL forced back on (PYTHON_GIL=1)",
)
def test_real_315t_with_gil_reenabled_is_unavailable():
    assert _sampler.probe_sampler().reason == _sampler.REASON_GIL_ENABLED


@needs_real_sampling
def test_real_sampler_is_available():
    probe = _sampler.probe_sampler()

    assert probe.available is True, probe.detail
    assert probe.mode == 1
    assert _sampler.Sampler.create().recheck_gil() is True


@needs_real_sampling
def test_real_sampler_captures_a_busy_thread_and_excludes_its_own():
    stop = threading.Event()

    def busy_worker(n):
        # Pure-Python load: no hashlib, no sleep, nothing that releases the GIL.
        # On the free-threaded build it is still on CPU every time the sampler looks.
        while not stop.is_set():
            total = 0
            for i in range(n):
                total += (i * 31) ^ (i >> 3)
            if total == -1:  # pragma: no cover - keep the loop live
                return

    worker = threading.Thread(target=busy_worker, args=(4000,), name="lo-busy", daemon=True)
    worker.start()
    try:
        sampler = _sampler.Sampler.create()
        assert sampler.available() is True
        window = sampler.collect_window(2.0)
    finally:
        stop.set()
        worker.join(timeout=5.0)

    assert window is not None
    assert window.samples > 0, "the CPU sampler produced no samples"
    assert window.counts, "aggregated stacks are empty"
    frames = [frame for stack in window.counts for frame in stack]
    # ``funcname`` is the qualname, so the nested worker shows up suffixed.
    assert any(frame[0].endswith("busy_worker") for frame in frames), frames
    sampler_file = os.path.realpath(_sampler.__file__)
    assert all(os.path.realpath(frame[1]) != sampler_file for frame in frames), frames

    data = sampler.encode_window(window)
    assert isinstance(data, bytes) and data.startswith(b"\x1f\x8b")


@needs_real_sampling
def test_real_sampler_prints_nothing(capfd):
    sampler = _sampler.Sampler.create()
    if not sampler.available():
        pytest.skip(sampler.reason())

    window = sampler.collect_window(0.2)
    assert window is not None
    time.sleep(0.01)

    captured = capfd.readouterr()
    assert captured.out == ""
