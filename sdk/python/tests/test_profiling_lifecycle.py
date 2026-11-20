"""Lifecycle of continuous profiling: switch, window loop, shutdown and fork.

The window loop (``lazyobserve._profiling.ProfilingController``) is driven with a
fake sampler, a fake clock and a fake uploader, so none of these tests samples the
real interpreter or waits for a real 10 s window. The sampler fake implements the
surface the loop uses from ``lazyobserve._sampler.Sampler``; a few tests use the
real ``Sampler`` around a fake ``sample()`` function to cover its interplay with the
loop (encoder errors, ``running``). Fork tests use a real ``os.fork``.
"""

from __future__ import annotations

import datetime
import json
import logging
import os
import signal
import threading
import time
import warnings
from types import SimpleNamespace

import pytest

import lazyobserve
from lazyobserve import _profiling, _sampler
from lazyobserve._profile_upload import CollectedProfile, ProfileUploader
from lazyobserve._sampler import SamplingWindow

LOGGER_NAME = "lazyobserve-test-profiling"
UNSET = object()

needs_fork = pytest.mark.skipif(not hasattr(os, "fork"), reason="needs os.fork")


@pytest.fixture(autouse=True)
def _fresh_warning_state():
    """One warning per class per process: forget what earlier tests emitted."""
    _profiling._reset_warning_state()
    yield
    _profiling._reset_warning_state()


# --- fakes ------------------------------------------------------------------


class FakeClock:
    def __init__(self):
        self.now = 1000.0

    def __call__(self):
        return self.now

    def advance(self, seconds):
        self.now += seconds


def make_window(marker="app", *, seconds=10.0, samples=1, failed=0, start_ns=1_700_000_000_000_000_000):
    return SamplingWindow(
        counts={((marker, "app.py", 1),): samples},
        start_ns=start_ns,
        duration_ns=int(seconds * 1_000_000_000),
        samples=samples,
        failed_samples=failed,
        attempts=samples + failed,
    )


class FakeSampler:
    """The part of ``Sampler`` the window loop talks to, scripted per window.

    ``steps`` are consumed one per ``collect_window`` call: a ``dict`` with
    ``window`` (``SamplingWindow`` or ``None``), ``failed`` ticks, ``advance``
    seconds on the fake clock (default: the requested duration) and ``disable``
    (a ``_sampler.REASON_*`` the sampler stops with). An exhausted script returns
    ``None``. ``gil`` is the answer queue for ``recheck_gil`` (default always fine).
    """

    def __init__(self, clock=None, *, steps=(), gil=(), available=True, reason=None, detail=""):
        self.clock = clock or FakeClock()
        self.steps = list(steps)
        self.gil = list(gil)
        self._available = available
        self._reason = reason
        self._detail = detail
        self._failed = 0
        self.failing_markers = set()  # windows whose encoding "raises" (encode_window -> None)
        self.calls = []
        self.stopped = threading.Event()

    def available(self):
        return self._available

    def reason(self):
        return self._reason

    def detail(self):
        return self._detail

    def disabled(self):
        return self._reason is not None

    def recheck_gil(self):
        self.calls.append(("recheck",))
        if not self._available:
            return False
        ok = self.gil.pop(0) if self.gil else True
        if not ok:
            self._available = False
            self._reason = _sampler.REASON_GIL_ENABLED
            self._detail = "the GIL was enabled after sampling started"
        return ok

    def collect_window(self, duration_sec=10.0):
        self.calls.append(("collect", duration_sec))
        self._failed = 0
        if not self.steps:
            return None
        step = self.steps.pop(0)
        self.clock.advance(step.get("advance", duration_sec))
        self._failed = step.get("failed", 0)
        if step.get("disable"):
            self._available = False
            self._reason = step["disable"]
            self._detail = "scripted"
        return step.get("window")

    def last_failed_ticks(self):
        return self._failed

    def encode_window(self, window):
        self.calls.append(("encode",))
        if window is None:
            return None
        marker = next(iter(window.counts))[0][0]
        if marker in self.failing_markers:
            return None
        return b"PB:" + marker.encode()

    def stop(self):
        self.stopped.set()


class BlockingSampler(FakeSampler):
    """``collect_window`` blocks like a real ``sample()`` until ``stop()``, then
    returns the partial window it has so far (``partial_seconds`` long)."""

    def __init__(self, *, partial_seconds=7.0, marker="partial", **kwargs):
        super().__init__(**kwargs)
        self.partial_seconds = partial_seconds
        self.marker = marker
        self.entered = threading.Event()

    def collect_window(self, duration_sec=10.0):
        self.calls.append(("collect", duration_sec))
        self.entered.set()
        assert self.stopped.wait(10), "stop() was never called"
        return make_window(self.marker, seconds=self.partial_seconds)


class FakeUploader:
    def __init__(self):
        self.items = []
        self.closed = False
        self.close_timeouts = []

    def enqueue(self, profile):
        self.items.append(profile)
        return True

    def close(self, timeout=None):
        self.closed = True
        self.close_timeouts.append(timeout)
        return None

    def dropped(self):
        return 0


@pytest.mark.parametrize(
    ("fake", "real", "methods"),
    [
        (
            FakeSampler,
            _sampler.Sampler,
            [
                "available",
                "reason",
                "detail",
                "disabled",
                "recheck_gil",
                "collect_window",
                "last_failed_ticks",
                "encode_window",
                "stop",
            ],
        ),
        (FakeUploader, ProfileUploader, ["enqueue", "close", "dropped"]),
    ],
)
def test_fakes_keep_the_signatures_of_what_they_stand_in_for(fake, real, methods):
    """A fake that drifted from the real surface would keep these tests green for
    a controller that no longer works against the real objects."""
    import inspect

    for name in methods:
        fake_params = list(inspect.signature(getattr(fake, name)).parameters)
        real_params = list(inspect.signature(getattr(real, name)).parameters)
        assert fake_params == real_params, name


def build(
    sampler,
    *,
    interval=60.0,
    cycles=1,
    uploader=None,
    new_upload_id=None,
):
    """Controller wired to fakes. The fake sleep records the requested delay,
    advances the fake clock and asks the loop to stop after ``cycles`` windows."""
    uploader = uploader or FakeUploader()
    delays = []
    drops = []
    ids = []

    def sleep(delay):
        delays.append(delay)
        sampler.clock.advance(delay)
        return len(delays) >= cycles

    def upload_id():
        value = new_upload_id() if new_upload_id else f"{len(ids) + 1:032x}"
        ids.append(value)
        return value

    controller = _profiling.ProfilingController(
        interval=interval,
        sampler_factory=lambda: sampler,
        uploader_factory=lambda: uploader,
        record_drop=lambda signal_, reason, count: drops.append((signal_, reason, count)),
        logger=logging.getLogger(LOGGER_NAME),
        clock=sampler.clock,
        sleep=sleep,
        new_upload_id=upload_id,
    )
    return SimpleNamespace(
        controller=controller, sampler=sampler, uploader=uploader, delays=delays, drops=drops, ids=ids
    )


def run(rig, timeout=5.0):
    rig.controller.start()
    assert rig.controller.join(timeout), "window loop did not finish"
    return rig


def warnings_of(caplog):
    return [r.getMessage() for r in caplog.records if r.name == LOGGER_NAME and r.levelno == logging.WARNING]


@pytest.fixture
def caplog_profiling(caplog):
    caplog.set_level(logging.DEBUG, logger=LOGGER_NAME)
    return caplog


# --- warning codes ------------------------------------------------------------


@pytest.mark.parametrize(
    ("reason", "code"),
    [
        (_sampler.REASON_PYTHON_TOO_OLD, "python_too_old"),
        (_sampler.REASON_PLATFORM, "unsupported_platform"),
        (_sampler.REASON_GIL_BUILD, "gil_build"),
        (_sampler.REASON_GIL_ENABLED, "gil_enabled"),
        (_sampler.REASON_IMPORT_FAILED, "probe_failed"),
        (_sampler.REASON_SIGNATURE_MISMATCH, "probe_failed"),
        (_sampler.REASON_MODE_MISSING, "probe_failed"),
        (_sampler.REASON_TRIAL_FAILED, "probe_failed"),
        (_sampler.REASON_DISABLED, "too_many_failures"),
        (_sampler.REASON_FAILURE_RATE, "sample_failure_rate"),
    ],
)
def test_sampler_reasons_map_to_stable_warning_codes(reason, code):
    assert _profiling.warning_code(reason) == code


def test_every_sampler_reason_has_a_code():
    reasons = {v for k, v in vars(_sampler).items() if k.startswith("REASON_")}
    assert reasons
    for reason in reasons:
        assert _profiling.warning_code(reason) != "internal_error", reason


# --- unavailable: one warning, with a code and a docs link ----------------------


@pytest.mark.parametrize(
    ("reason", "code"),
    [
        (_sampler.REASON_PYTHON_TOO_OLD, "python_too_old"),
        (_sampler.REASON_PLATFORM, "unsupported_platform"),
        (_sampler.REASON_GIL_BUILD, "gil_build"),
        (_sampler.REASON_GIL_ENABLED, "gil_enabled"),
        (_sampler.REASON_TRIAL_FAILED, "probe_failed"),
    ],
)
def test_unavailable_warns_once_with_the_reason_code_and_never_samples(reason, code, caplog_profiling):
    sampler = FakeSampler(available=False, reason=reason, detail="nope")
    rig = run(build(sampler))

    messages = warnings_of(caplog_profiling)
    assert len(messages) == 1
    assert f"reason={code}" in messages[0]
    assert _profiling.DOC_URL in messages[0]
    assert "profiling" in messages[0].lower()
    assert sampler.calls == []  # no GIL recheck, no window
    assert rig.uploader.items == []
    assert rig.delays == []
    assert rig.controller.shutdown(1.0) is None


def test_a_warning_class_is_emitted_once_per_process_across_controllers(caplog_profiling):
    for _ in range(3):
        run(build(FakeSampler(available=False, reason=_sampler.REASON_GIL_BUILD, detail="a")))
    run(build(FakeSampler(available=False, reason=_sampler.REASON_PLATFORM, detail="b")))

    messages = warnings_of(caplog_profiling)
    assert len(messages) == 2
    assert "reason=gil_build" in messages[0]
    assert "reason=unsupported_platform" in messages[1]


def test_a_probe_that_raises_is_a_probe_failed_warning_not_an_exception(caplog_profiling):
    def boom():
        raise RuntimeError("probe exploded")

    controller = _profiling.ProfilingController(
        interval=60.0,
        sampler_factory=boom,
        uploader_factory=FakeUploader,
        record_drop=lambda *a: None,
        logger=logging.getLogger(LOGGER_NAME),
    )
    controller.start()
    assert controller.join(5)
    messages = warnings_of(caplog_profiling)
    assert len(messages) == 1
    assert "reason=probe_failed" in messages[0]
    assert controller.shutdown(1.0) is None


# --- cadence ------------------------------------------------------------------


def test_default_cadence_is_a_10_second_window_every_60_seconds():
    sampler = FakeSampler(steps=[{"window": make_window(f"w{i}")} for i in range(3)])
    rig = run(build(sampler, cycles=3))

    assert [c for c in sampler.calls if c[0] == "collect"] == [("collect", 10.0)] * 3
    # Interval is start-to-start: 60 s minus the 10 s the window already took.
    assert rig.delays == [50.0, 50.0, 50.0]
    assert len(rig.uploader.items) == 3


def test_interval_below_the_window_length_runs_windows_back_to_back():
    sampler = FakeSampler(steps=[{"window": make_window(f"w{i}")} for i in range(4)])
    rig = run(build(sampler, interval=5.0, cycles=4))

    assert [c for c in sampler.calls if c[0] == "collect"] == [("collect", 10.0)] * 4
    assert rig.delays == [0.0, 0.0, 0.0, 0.0]  # never negative, never a gap
    assert len(rig.uploader.items) == 4


def test_the_first_window_starts_immediately():
    sampler = FakeSampler(steps=[{"window": make_window()}])
    rig = run(build(sampler))
    assert sampler.calls[0] == ("recheck",)
    assert sampler.calls[1] == ("collect", 10.0)
    assert rig.delays  # the sleep only follows the first window


# --- GIL recheck before every window ------------------------------------------------


def test_gil_is_rechecked_before_every_window_and_a_late_gil_stops_profiling(caplog_profiling):
    sampler = FakeSampler(steps=[{"window": make_window("w1")}, {"window": make_window("w2")}], gil=[True, True, False])
    rig = run(build(sampler, cycles=10))

    assert sampler.calls == [
        ("recheck",),
        ("collect", 10.0),
        ("encode",),
        ("recheck",),
        ("collect", 10.0),
        ("encode",),
        ("recheck",),  # the GIL is back: no third window
    ]
    assert len(rig.uploader.items) == 2
    messages = warnings_of(caplog_profiling)
    assert len(messages) == 1
    assert "reason=gil_enabled" in messages[0]
    assert _profiling.DOC_URL in messages[0]
    assert len(rig.delays) == 2  # it did not sleep into another cycle


def test_a_disabled_sampler_never_gets_another_window(caplog_profiling):
    sampler = FakeSampler(
        steps=[
            {"window": None, "failed": 60, "disable": _sampler.REASON_FAILURE_RATE},
            {"window": make_window("never")},
        ]
    )
    rig = run(build(sampler, cycles=10))

    assert len([c for c in sampler.calls if c[0] == "collect"]) == 1
    assert rig.uploader.items == []
    messages = warnings_of(caplog_profiling)
    assert len(messages) == 1
    assert "reason=sample_failure_rate" in messages[0]
    assert rig.delays == []  # no waiting around for a sampler that stays off


def test_three_failed_windows_warn_once_and_stay_disabled(caplog_profiling):
    sampler = FakeSampler(
        steps=[
            {"window": None},
            {"window": None},
            {"window": None, "disable": _sampler.REASON_DISABLED},
            {"window": make_window("never")},
        ]
    )
    rig = run(build(sampler, cycles=10))
    assert len(rig.delays) == 2  # slept after the 1st and 2nd failure only
    messages = warnings_of(caplog_profiling)
    assert len(messages) == 1
    assert "reason=too_many_failures" in messages[0]


# --- failed ticks and discarded windows --------------------------------------------


def test_failed_ticks_are_counted_as_sample_failed_and_all_failed_windows_are_not_uploaded():
    sampler = FakeSampler(
        steps=[
            {"window": None, "failed": 100},  # every tick failed: sampler returns no window
            {"window": make_window("ok", samples=97, failed=3), "failed": 3},
            {"window": make_window("clean")},
        ]
    )
    rig = run(build(sampler, cycles=3))

    assert rig.drops == [("profile", "sample_failed", 100), ("profile", "sample_failed", 3)]
    assert [p.data for p in rig.uploader.items] == [b"PB:ok", b"PB:clean"]
    assert sampler.calls.count(("encode",)) == 2  # the all-failed window was never encoded


def test_a_window_over_the_failure_rate_is_dropped_but_its_ticks_still_count(caplog_profiling):
    sampler = FakeSampler(steps=[{"window": None, "failed": 60, "disable": _sampler.REASON_FAILURE_RATE}])
    rig = run(build(sampler))
    assert rig.drops == [("profile", "sample_failed", 60)]
    assert rig.uploader.items == []


def test_no_telemetry_drop_when_no_tick_failed():
    sampler = FakeSampler(steps=[{"window": make_window()}])
    rig = run(build(sampler))
    assert rig.drops == []


# --- upload ------------------------------------------------------------------------


def test_upload_id_is_generated_when_the_window_is_encoded_not_before():
    sampler = FakeSampler(
        steps=[
            {"window": None},  # nothing collected
            {"window": make_window("encode-fails")},
            {"window": make_window("ok")},
        ]
    )
    sampler.failing_markers.add("encode-fails")
    rig = build(sampler, cycles=3)
    ids_drawn_before_encode = []
    original = sampler.encode_window

    def encode(window):
        ids_drawn_before_encode.append(len(rig.ids))
        return original(window)

    sampler.encode_window = encode
    run(rig)

    # An id is drawn only for a window that was collected *and* encoded ...
    assert rig.ids == ["0" * 31 + "1"]
    assert [p.upload_id for p in rig.uploader.items] == rig.ids
    # ... and only once encoding is done: none was drawn at window start or by a
    # failed window (two windows reached the encoder, both before any draw).
    assert ids_drawn_before_encode == [0, 0]


def test_each_uploaded_window_carries_its_own_id_type_and_span():
    start_ns = 1_700_000_000_123_456_000
    sampler = FakeSampler(
        steps=[
            {"window": make_window("a", start_ns=start_ns)},
            {"window": make_window("b", start_ns=start_ns + 60_000_000_000)},
        ]
    )
    rig = run(build(sampler, cycles=2))

    first, second = rig.uploader.items
    assert isinstance(first, CollectedProfile)
    assert first.upload_id != second.upload_id
    assert len(first.upload_id) == 32
    assert first.type_name == "cpu"
    assert first.data == b"PB:a"
    assert first.start == datetime.datetime(2023, 11, 14, 22, 13, 20, 123456, tzinfo=datetime.timezone.utc)
    assert first.end - first.start == datetime.timedelta(seconds=10)
    assert second.start - first.start == datetime.timedelta(seconds=60)


def test_real_upload_ids_are_32_hex_and_unique_across_windows():
    from lazyobserve._profile_upload import new_upload_id

    sampler = FakeSampler(steps=[{"window": make_window(f"w{i}")} for i in range(5)])
    rig = run(build(sampler, cycles=5, new_upload_id=new_upload_id))
    ids = [p.upload_id for p in rig.uploader.items]
    assert len(ids) == len(set(ids)) == 5
    assert all(len(i) == 32 and int(i, 16) >= 0 for i in ids)


# --- errors never escape --------------------------------------------------------------


def test_an_unexpected_error_in_the_loop_is_contained_and_warned_once(caplog_profiling):
    class Exploding(FakeSampler):
        def collect_window(self, duration_sec=10.0):
            raise RuntimeError("bug in the sampler")

    rig = run(build(Exploding()))
    messages = warnings_of(caplog_profiling)
    assert len(messages) == 1
    assert "reason=internal_error" in messages[0]
    assert rig.controller.shutdown(1.0) is None


def _real_sampler(encoder, *, hold_until_stopped=False):
    """The real ``Sampler`` around a fake ``sample()`` that yields one stack."""
    entered = threading.Event()

    def sample(pid, collector, *, duration_sec=None, all_threads=False, mode=0):
        entered.set()
        if hold_until_stopped:
            deadline = time.monotonic() + 10
            while collector.running and time.monotonic() < deadline:
                time.sleep(0.001)
        frame = ("app.py", (1, 1, 0, 0), "work", None)
        thread = SimpleNamespace(thread_id=1, frame_info=[frame])
        collector.collect([SimpleNamespace(threads=[thread])], timestamps_us=[0] * 3)

    sampler = _sampler.Sampler(
        _sampler.SamplerProbe(True, mode=1, sample=sample),
        encoder=encoder,
        gil_enabled=lambda: False,  # whatever interpreter runs the suite
    )
    return sampler, entered


@pytest.mark.parametrize("error", [TypeError("bad frame"), ValueError("bad frame")])
def test_encoder_errors_stay_in_the_loop_and_three_in_a_row_disable_profiling(error, caplog_profiling):
    def encoder(*args, **kwargs):
        raise error

    sampler, _ = _real_sampler(encoder)
    uploader = FakeUploader()
    sleeps = []
    controller = _profiling.ProfilingController(
        interval=0.0,
        sampler_factory=lambda: sampler,
        uploader_factory=lambda: uploader,
        record_drop=lambda *a: None,
        logger=logging.getLogger(LOGGER_NAME),
        sleep=lambda delay: sleeps.append(delay) or False,
    )
    controller.start()
    assert controller.join(5)

    assert uploader.items == []
    assert sampler.disabled()
    assert len(sleeps) == 2
    messages = warnings_of(caplog_profiling)
    assert len(messages) == 1
    assert "reason=too_many_failures" in messages[0]
    assert controller.shutdown(1.0) is None


@pytest.mark.parametrize("error", [TypeError("bad frame"), ValueError("bad frame")])
def test_encoder_errors_never_reach_the_shutdown_path(error):
    def encoder(*args, **kwargs):
        raise error

    sampler, entered = _real_sampler(encoder, hold_until_stopped=True)
    uploader = FakeUploader()
    controller = _profiling.ProfilingController(
        interval=60.0,
        sampler_factory=lambda: sampler,
        uploader_factory=lambda: uploader,
        record_drop=lambda *a: None,
        logger=logging.getLogger(LOGGER_NAME),
        # Let the (tiny) partial window through the 5 s rule: the encoder is what fails.
        min_shutdown_window=0.0,
    )
    controller.start()
    assert entered.wait(5)

    assert controller.shutdown(5.0) is None  # no exception, no error result
    assert controller.join(5)
    assert uploader.items == []
    assert sampler.consecutive_failures() == 1


# --- shutdown --------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("partial_seconds", "uploaded"),
    [(0.0, False), (4.999, False), (5.0, True), (7.5, True), (9.9, True)],
)
def test_shutdown_stops_sampling_at_once_and_uploads_only_a_window_of_5_seconds_or_more(
    partial_seconds, uploaded
):
    sampler = BlockingSampler(partial_seconds=partial_seconds)
    rig = build(sampler)
    rig.controller.start()
    assert sampler.entered.wait(5)

    began = time.monotonic()
    err = rig.controller.shutdown(5.0)
    elapsed = time.monotonic() - began

    assert err is None
    assert sampler.stopped.is_set()  # sampling was told to stop, not waited out
    assert elapsed < 2.0
    assert len(rig.uploader.items) == (1 if uploaded else 0)  # uploaded inside the shutdown call
    assert rig.uploader.closed
    assert rig.controller.join(1)
    assert rig.delays == []  # no further cycle after the stop


def test_shutdown_closes_the_uploader_with_what_is_left_of_the_budget():
    sampler = BlockingSampler(partial_seconds=8.0)
    rig = build(sampler)
    rig.controller.start()
    assert sampler.entered.wait(5)
    rig.controller.shutdown(3.0)
    assert len(rig.uploader.close_timeouts) == 1
    assert 0.0 <= rig.uploader.close_timeouts[0] <= 3.0


def test_shutdown_is_idempotent_and_does_not_resurrect_the_loop():
    sampler = BlockingSampler(partial_seconds=8.0)
    rig = build(sampler)
    rig.controller.start()
    assert sampler.entered.wait(5)
    assert rig.controller.shutdown(3.0) is None
    assert rig.controller.shutdown(3.0) is None
    assert len(rig.uploader.items) == 1
    assert len(rig.uploader.close_timeouts) == 1
    rig.controller.start()  # a second start after shutdown is a no-op
    assert not rig.controller.is_running()


def test_shutdown_before_the_probe_finished_leaves_nothing_running():
    gate = threading.Event()

    def slow_factory():
        gate.wait(5)
        return FakeSampler(steps=[{"window": make_window()}])

    uploads = []
    controller = _profiling.ProfilingController(
        interval=60.0,
        sampler_factory=slow_factory,
        uploader_factory=lambda: uploads.append(FakeUploader()) or uploads[-1],
        record_drop=lambda *a: None,
        logger=logging.getLogger(LOGGER_NAME),
    )
    controller.start()
    assert controller.is_running()
    timer = threading.Timer(0.1, gate.set)
    timer.start()
    assert controller.shutdown(5.0) is None
    assert controller.join(5)
    timer.join()
    # No window was started after the stop request, so nothing was uploaded.
    assert all(not u.items for u in uploads)


def test_shutdown_does_not_leak_threads():
    import httpx

    def names():
        return {t.name for t in threading.enumerate() if t.name.startswith("lazyobserve-profil")}

    before = names()
    sampler = BlockingSampler(partial_seconds=6.0)
    opened = []

    def handler(request: httpx.Request) -> httpx.Response:
        opened.append(request)
        return httpx.Response(201, content=b"")

    controller = _profiling.ProfilingController(
        interval=60.0,
        sampler_factory=lambda: sampler,
        uploader_factory=lambda: ProfileUploader(
            "http://127.0.0.1:1", "tok", "svc", transport=httpx.MockTransport(handler)
        ),
        record_drop=lambda *a: None,
        logger=logging.getLogger(LOGGER_NAME),
    )
    controller.start()
    assert sampler.entered.wait(5)
    assert controller.shutdown(5.0) is None
    assert controller.join(5)
    deadline = time.monotonic() + 5
    while names() - before and time.monotonic() < deadline:
        time.sleep(0.01)
    assert names() - before == set()
    assert len(opened) == 1  # the 6 s partial window went out through the real queue
    assert str(opened[0].url).endswith("/api/profiles")


# --- public switch through init/build_client -------------------------------------------


def _init(**overrides):
    # Traces only and no spans: the metrics reader would try to export on shutdown
    # to the unreachable DSN and make every test wait for its retries.
    fields = {"dsn": "http://127.0.0.1:1/p/default@TOKEN", "service_name": "lifecycle", "traces": True}
    fields.update(overrides)
    return lazyobserve.init(**fields)


def _profiling_threads():
    return [t for t in threading.enumerate() if t.name == "lazyobserve-profiling" and t.is_alive()]


def test_profiling_is_off_by_default(monkeypatch):
    monkeypatch.setattr(
        _sampler, "probe_sampler", lambda: pytest.fail("probed although profiling is off")
    )
    lo = _init()
    try:
        assert lo.profiling is None
        assert _profiling_threads() == []
    finally:
        lo.shutdown(timeout=1.0)


def test_build_client_warns_once_with_a_code_when_this_interpreter_cannot_sample(monkeypatch, caplog):
    caplog.set_level(logging.WARNING, logger="lazyobserve")
    monkeypatch.setattr(
        _sampler,
        "probe_sampler",
        lambda: _sampler.SamplerProbe(False, reason=_sampler.REASON_GIL_BUILD, detail="not free-threaded"),
    )
    lo = _init(profiling=True)
    try:
        assert lo.profiling is not None
        assert lo.profiling.join(5)
        messages = [r.getMessage() for r in caplog.records if r.name == "lazyobserve" and "profiling" in r.getMessage().lower()]
        assert len(messages) == 1
        assert "reason=gil_build" in messages[0]
        assert _profiling.DOC_URL in messages[0]
        assert not lo.degraded
    finally:
        lo.shutdown(timeout=1.0)
    assert _profiling_threads() == []


@pytest.mark.parametrize(("configured", "expected"), [(None, 60.0), (5, 5.0), (0, 60.0), (-3, 60.0)])
def test_build_client_hands_the_normalized_interval_to_the_loop(monkeypatch, configured, expected):
    monkeypatch.setattr(
        _sampler, "probe_sampler", lambda: _sampler.SamplerProbe(False, reason=_sampler.REASON_PLATFORM)
    )
    extra = {} if configured is None else {"profiling_interval": configured}
    lo = _init(profiling=True, **extra)
    try:
        assert lo.profiling.interval == expected
    finally:
        lo.shutdown(timeout=1.0)


def test_build_client_does_not_wait_for_the_probe(monkeypatch):
    release = threading.Event()
    probing = threading.Event()

    def slow_probe():
        probing.set()
        release.wait(10)
        return _sampler.SamplerProbe(False, reason=_sampler.REASON_TRIAL_FAILED, detail="late")

    monkeypatch.setattr(_sampler, "probe_sampler", slow_probe)
    began = time.monotonic()
    lo = _init(profiling=True)
    try:
        returned_after = time.monotonic() - began
        assert probing.wait(5)  # the probe did start ...
        assert returned_after < 3.0  # ... but init() did not sit through it
        assert lo.profiling.is_running()
    finally:
        release.set()
        lo.shutdown(timeout=2.0)


def test_build_client_survives_a_profiling_start_failure(monkeypatch):
    def boom(*args, **kwargs):
        raise RuntimeError("cannot start")

    monkeypatch.setattr(_profiling, "start_profiling", boom)
    lo = _init(profiling=True)
    try:
        assert not lo.degraded  # a broken optional signal must not degrade the client
        assert lo.profiling is None
    finally:
        lo.shutdown(timeout=1.0)


def test_profile_uploader_drops_are_counted_and_logged(monkeypatch, caplog):
    from tests.helpers import make_client

    caplog.set_level(logging.WARNING, logger=LOGGER_NAME)
    client = make_client()
    captured = {}

    class RecordingUploader:
        def __init__(self, **kwargs):
            captured.update(kwargs)

    monkeypatch.setattr(_profiling, "ProfileUploader", RecordingUploader)
    parsed = SimpleNamespace(endpoint="http://gw:3100", token="lo_tok")
    _profiling.build_uploader(client, parsed, client.config, logging.getLogger(LOGGER_NAME))

    assert captured["endpoint"] == "http://gw:3100"
    assert captured["token"] == "lo_tok"
    assert captured["service"] == client.config.service_name
    captured["warn"]("queue_full", None, 2)

    assert client.diagnostics().dropped_telemetry == 2
    assert any("reason=queue_full" in m for m in warnings_of(caplog))
    client.shutdown(timeout=1.0)


def test_client_shutdown_stops_profiling_within_its_budget():
    from tests.helpers import make_client

    class FakeProfiling:
        def __init__(self, result=None, error=None):
            self.timeouts = []
            self.result = result
            self.error = error

        def shutdown(self, timeout):
            self.timeouts.append(timeout)
            if self.error:
                raise self.error
            return self.result

    client = make_client()
    client.profiling = FakeProfiling()
    assert client.shutdown(timeout=3.0) is None
    assert len(client.profiling.timeouts) == 1
    assert 0.0 <= client.profiling.timeouts[0] <= 3.0

    failing = make_client()
    failing.profiling = FakeProfiling(result=RuntimeError("upload queue stuck"))
    assert "upload queue stuck" in str(failing.shutdown(timeout=1.0))

    raising = make_client()
    raising.profiling = FakeProfiling(error=ValueError("boom"))
    err = raising.shutdown(timeout=1.0)  # returned, never raised
    assert err is not None
    assert "boom" in str(err)


# --- fork ---------------------------------------------------------------------------------


def run_in_child(fn, timeout=15):
    """Run ``fn`` in a forked child and return ``(wait status, JSON payload)``.

    A SIGALRM watchdog kills a child that deadlocks, which shows up as a signalled
    exit status instead of a hung test run.
    """
    read_fd, write_fd = os.pipe()
    with warnings.catch_warnings():
        warnings.simplefilter("ignore")  # "fork() in a multi-threaded process" (3.12+)
        pid = os.fork()
    if pid == 0:
        code = 1
        try:
            os.close(read_fd)
            signal.alarm(timeout)
            payload = fn()
            os.write(write_fd, json.dumps(payload).encode())
            code = 0
        except BaseException as exc:  # noqa: BLE001 - report instead of hanging the parent
            try:
                os.write(write_fd, json.dumps({"error": repr(exc)}).encode())
            except BaseException:  # noqa: BLE001
                pass
        finally:
            os._exit(code)
    os.close(write_fd)
    chunks = []
    while True:
        data = os.read(read_fd, 65536)
        if not data:
            break
        chunks.append(data)
    os.close(read_fd)
    _, status = os.waitpid(pid, 0)
    raw = b"".join(chunks)
    return status, json.loads(raw) if raw else None


def assert_child_ok(status, payload):
    assert not os.WIFSIGNALED(status), f"child killed by signal {os.WTERMSIG(status)} (deadlock?)"
    assert os.WEXITSTATUS(status) == 0, payload
    assert payload is not None and "error" not in payload, payload
    return payload


class _Response:
    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False

    def read(self, n=-1):
        return b""


def _multipart_field(body, name):
    marker = f'name="{name}"\r\n\r\n'.encode()
    start = body.index(marker) + len(marker)
    return body[start : body.index(b"\r\n", start)].decode()


class ForkRig:
    """A real ``ProfileUploader`` over a fake opener plus samplers that mimic a
    window in flight: the parent's second window blocks mid-flight (this is when
    the fork happens); every sampler the factory builds after that (the child's)
    returns one window and then blocks until it is stopped."""

    def __init__(self):
        self.parent_pid = os.getpid()
        self.sent = []  # (pid, request body) for every upload attempt
        self.gate = threading.Event()  # holds the parent's first upload in flight
        self.entered = threading.Event()  # parent is inside its second window
        self.release = threading.Event()
        self.samplers = []
        self.uploaders = []

    def handler(self, request):
        import httpx

        self.sent.append((os.getpid(), request.content))
        if os.getpid() == self.parent_pid:
            self.gate.wait(10)
        return httpx.Response(201, content=b"")

    def make_uploader(self):
        import httpx

        uploader = ProfileUploader(
            "http://127.0.0.1:1",
            "tok",
            "fork-svc",
            max_attempts=1,
            transport=httpx.MockTransport(self.handler),
        )
        self.uploaders.append(uploader)
        return uploader

    def make_sampler(self):
        rig = self
        index = len(self.samplers)

        class Sampler(FakeSampler):
            def collect_window(self, duration_sec=10.0):
                self.calls.append(("collect", duration_sec))
                nth = len([c for c in self.calls if c[0] == "collect"])
                if index == 0:  # the parent's sampler
                    if nth == 1:
                        return make_window("pre_fork_queued")
                    if nth == 2:
                        rig.entered.set()
                        assert rig.release.wait(10)
                        return make_window("pre_fork_window")
                elif nth == 1:  # the child's sampler
                    return make_window("child_window")
                self.stopped.wait(10)
                return None

        sampler = Sampler()
        self.samplers.append(sampler)
        return sampler

    def controller(self):
        return _profiling.ProfilingController(
            interval=0.0,
            sampler_factory=self.make_sampler,
            uploader_factory=self.make_uploader,
            record_drop=lambda *a: None,
            logger=logging.getLogger(LOGGER_NAME),
        )


def _uploads_since(rig, n0, pid):
    return [body for p, body in rig.sent[n0:] if p == pid]


@needs_fork
def test_fork_in_the_middle_of_a_window_gives_the_child_a_fresh_window_and_its_own_upload_id():
    rig = ForkRig()
    controller = rig.controller()
    controller.start()
    try:
        assert rig.entered.wait(5)
        # The parent's first window is queued and its upload is stuck in flight (gate)
        # while the second window is in the middle of sampling: the fork lands here.
        n0 = len(rig.sent)

        def child():
            assert controller.is_running()  # the sampling thread was rebuilt and restarted
            deadline = time.monotonic() + 8
            while len(_uploads_since(rig, n0, os.getpid())) < 1 and time.monotonic() < deadline:
                time.sleep(0.01)
            err = controller.shutdown(3.0)
            return {
                "err": repr(err),
                "bodies": [b.decode("latin-1") for b in _uploads_since(rig, n0, os.getpid())],
                "running": controller.is_running(),
            }

        payload = assert_child_ok(*run_in_child(child))

        assert payload["err"] == "None"
        assert len(payload["bodies"]) == 1  # exactly the child's own window
        child_body = payload["bodies"][0]
        assert "child_window" in child_body
        assert "pre_fork_window" not in child_body  # the window in flight at fork time is not re-uploaded
        assert "pre_fork_queued" not in child_body  # nor what the parent still had queued
        assert payload["running"] is False

        rig.release.set()
        rig.gate.set()
        assert controller.shutdown(5.0) is None
    finally:
        rig.release.set()
        rig.gate.set()
        controller.shutdown(2.0)

    parent_bodies = _uploads_since(rig, 0, rig.parent_pid)
    parent_ids = {_multipart_field(b, "upload_id") for b in parent_bodies}
    assert len(parent_bodies) >= 2  # the queued window and the one that was in flight
    child_id = _multipart_field(payload["bodies"][0].encode("latin-1"), "upload_id")
    assert len(child_id) == 32
    assert child_id not in parent_ids  # parent and child never share an upload_id
    assert any(b"pre_fork_window" in b for b in parent_bodies)  # the parent still uploads its own


@needs_fork
def test_a_child_forked_while_a_lock_is_held_does_not_deadlock():
    rig = ForkRig()
    controller = rig.controller()
    controller.start()
    holding = threading.Event()
    done = threading.Event()
    try:
        assert rig.entered.wait(5)
        old_uploader = rig.uploaders[0]

        def hold_locks():
            # Locks another parent thread happens to hold at fork time. The child
            # inherits them locked, with no thread left to release them.
            with controller._lock, old_uploader.delivery._close_lock:
                holding.set()
                done.wait(20)

        holder = threading.Thread(target=hold_locks, daemon=True)
        holder.start()
        assert holding.wait(5)

        def child():
            err = controller.shutdown(3.0)  # takes the controller's lock, uses its uploader
            return {"err": repr(err)}

        payload = assert_child_ok(*run_in_child(child))
        assert payload["err"] == "None"
    finally:
        done.set()
        rig.release.set()
        rig.gate.set()
        controller.shutdown(2.0)


@needs_fork
def test_the_fork_hook_survives_into_grandchildren():
    rig = ForkRig()
    controller = rig.controller()
    controller.start()
    try:
        assert rig.entered.wait(5)

        def child():
            def grandchild():
                running = controller.is_running()
                controller.shutdown(2.0)
                return {"running": running}

            status, payload = run_in_child(grandchild)
            controller.shutdown(2.0)
            return {"status": status, "payload": payload}

        outer = assert_child_ok(*run_in_child(child))
        inner = assert_child_ok(outer["status"], outer["payload"])
        assert inner["running"] is True
    finally:
        rig.release.set()
        rig.gate.set()
        controller.shutdown(2.0)


@needs_fork
def test_a_controller_that_was_shut_down_or_disabled_is_not_restarted_in_the_child():
    gone = build(FakeSampler(steps=[{"window": make_window()}]))
    run(gone)
    gone.controller.shutdown(1.0)

    disabled = build(FakeSampler(available=False, reason=_sampler.REASON_GIL_BUILD))
    run(disabled)

    def child():
        return {
            "shutdown_running": gone.controller.is_running(),
            "disabled_running": disabled.controller.is_running(),
        }

    payload = assert_child_ok(*run_in_child(child))
    assert payload == {"shutdown_running": False, "disabled_running": False}


@needs_fork
def test_warning_dedupe_is_per_process_so_a_child_may_warn_again(caplog_profiling):
    run(build(FakeSampler(available=False, reason=_sampler.REASON_GIL_BUILD, detail="a")))
    assert len(warnings_of(caplog_profiling)) == 1

    def child():
        records = []

        class Capture(logging.Handler):
            def emit(self, record):
                records.append(record.getMessage())

        logger = logging.getLogger(LOGGER_NAME)
        handler = Capture(level=logging.WARNING)
        logger.addHandler(handler)
        try:
            run(build(FakeSampler(available=False, reason=_sampler.REASON_GIL_BUILD, detail="a")))
        finally:
            logger.removeHandler(handler)
        return {"messages": records}

    payload = assert_child_ok(*run_in_child(child))
    assert len(payload["messages"]) == 1
    assert "reason=gil_build" in payload["messages"][0]


def test_track_is_thread_safe_prunes_dead_refs_and_registers_fork_hook_once(monkeypatch):
    import gc

    registrations = []
    monkeypatch.setattr(_profiling, "_controllers", [])
    monkeypatch.setattr(_profiling, "_fork_hook_registered", False)
    monkeypatch.setattr(
        os,
        "register_at_fork",
        lambda **kwargs: registrations.append(kwargs),
    )

    barrier = threading.Barrier(8)
    controllers = []

    def worker():
        barrier.wait()
        for _ in range(5):
            ctrl = build(FakeSampler(available=False)).controller
            controllers.append(ctrl)
            _profiling._track(ctrl)

    threads = [threading.Thread(target=worker) for _ in range(8)]
    for t in threads:
        t.start()
    for t in threads:
        t.join(timeout=5)

    assert len(registrations) == 1
    controllers.clear()
    gc.collect()
    live = build(FakeSampler(available=False)).controller
    _profiling._track(live)
    assert len(_profiling._controllers) == 1


@needs_fork
def test_fork_while_controllers_lock_is_held_does_not_deadlock():
    holding = threading.Event()
    done = threading.Event()

    def hold_lock():
        with _profiling._controllers_lock:
            holding.set()
            done.wait(20)

    holder = threading.Thread(target=hold_lock, daemon=True)
    holder.start()
    assert holding.wait(5)
    try:
        def child():
            ctrl = build(FakeSampler(available=False)).controller
            _profiling._track(ctrl)
            return {"ok": True}

        payload = assert_child_ok(*run_in_child(child))
        assert payload == {"ok": True}
    finally:
        done.set()
        holder.join(5)


# --- real interpreter (free-threaded 3.15 only) ---------------------------------------------


def _real_sampling_possible():
    import sys
    import sysconfig

    return (
        sys.version_info >= (3, 15)
        and sys.platform == "linux"
        and bool(sysconfig.get_config_var("Py_GIL_DISABLED"))
        and getattr(sys, "_is_gil_enabled", lambda: True)() is False
    )


@pytest.mark.skipif(not _real_sampling_possible(), reason="needs Linux + free-threaded CPython 3.15+ without GIL")
def test_real_315t_profiles_a_busy_thread_and_uploads_through_the_real_path():
    """The whole chain on a real interpreter: real probe, real ``sample()``, real
    encoder, real ``ProfileUploader`` (fake opener). Shutdown cuts a window that is
    already past 5 s, so the test takes about 6 seconds."""
    stop = threading.Event()

    def burn_cpu_for_lifecycle_test():
        total = 0
        while not stop.is_set():
            for i in range(20_000):
                total += i * i
        return total

    import httpx

    sent = []

    def handler(request: httpx.Request) -> httpx.Response:
        sent.append(request)
        return httpx.Response(201, content=b"")

    probed = threading.Event()

    def create_sampler():
        sampler = _sampler.Sampler.create()
        probed.set()  # the first window starts right after the probe
        return sampler

    worker = threading.Thread(target=burn_cpu_for_lifecycle_test, name="burner", daemon=True)
    worker.start()
    controller = _profiling.ProfilingController(
        interval=60.0,
        sampler_factory=create_sampler,
        uploader_factory=lambda: ProfileUploader(
            "http://127.0.0.1:1", "tok", "real-svc", transport=httpx.MockTransport(handler)
        ),
        record_drop=lambda *a: None,
        logger=logging.getLogger(LOGGER_NAME),
    )
    try:
        controller.start()
        # Time the 6 s from the end of the probe, not from start(): a slow import of
        # profiling.sampling on a loaded runner must not push the window under 5 s.
        assert probed.wait(30)
        time.sleep(6.0)
        assert controller.shutdown(10.0) is None
    finally:
        stop.set()
        worker.join(5)

    assert len(sent) == 1
    body = sent[0].content
    assert _multipart_field(body, "type") == "cpu"
    assert _multipart_field(body, "service") == "real-svc"
    import gzip

    start = body.index(b"\x1f\x8b")
    raw = gzip.decompress(body[start : body.rindex(b"\r\n--")])
    assert b"burn_cpu_for_lifecycle_test" in raw
