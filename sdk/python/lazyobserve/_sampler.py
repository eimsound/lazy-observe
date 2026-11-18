"""In-process CPU sampler built on CPython 3.15+ ``profiling.sampling``.

The sampler targets *this* process (``os.getpid()``) from a thread inside it, so
it needs no ``ptrace`` permission and never touches the network. It is entered
through :func:`probe_sampler`, which answers "can this interpreter be sampled at
all" instead of maintaining a version whitelist:

1. below CPython 3.15 nothing is imported at all (3.9-3.14 stayed at "no
   sampler", not "private ``_remote_debugging``");
2. three runtime gates (ADR 0014, revised): ``sys.platform == "linux"``, a
   free-threaded build (``Py_GIL_DISABLED``), and ``sys._is_gil_enabled()``
   returning False (a missing attribute counts as "enabled"). They run before the
   private import and before the trial sample on purpose: on a GIL build the
   trial raises nothing and just returns no samples, so it cannot detect this;
3. ``profiling.sampling.sample.sample`` must accept ``pid``, ``collector``,
   ``all_threads``, ``mode`` and ``duration_sec`` and
   ``profiling.sampling.constants.PROFILING_MODE_CPU`` must exist;
4. one short trial sample must complete without raising.

Anything else yields ``available=False`` plus a stable reason code, and the
caller only warns. The GIL can come back later (a C extension imported after
startup), so the window loop calls :meth:`Sampler.recheck_gil` before each window.

Measured facts about 3.15.2 (see ``.agents/docs/research/python-315-sampling.md``
and the ADR). They are why this module looks the way it does:

* the sample interval is read from ``collector.sample_interval_usec``; there is
  no interval argument on ``sample()``. 100 Hz means 10_000 us;
* frames are ``(filename, location, funcname, opcode)`` tuples; ``location`` is
  a ``LocationInfo`` namedtuple whose first item is the line (``None`` for
  synthetic frames), and ``funcname`` *is* the qualname (``Outer.method``,
  ``Outer.method.<locals>.inner``). Frames arrive leaf-first;
* ``thread_id`` is the OS thread id (``threading.get_native_id()``), not
  ``threading.get_ident()``. The sampler therefore excludes itself by native id;
* an aggregating collector is called once per distinct stack with
  ``timestamps_us`` holding one entry per collapsed sample, so the weight of a
  stack is ``len(timestamps_us)``;
* ``SampleProfiler.sample`` prints its statistics to stdout. We replace the
  module-level ``print`` of ``profiling.sampling.sample`` with a no-op so the
  SDK never writes into the application's stdout.

Failure policy (owner decision): sampling and encoding errors are caught in the
sampling thread and counted; after ``MAX_CONSECUTIVE_FAILURES`` consecutive
failures the sampler disables itself and reports one warning. A window is a
``collect_window`` plus its ``encode_window``; only a successful encode ends a
failure streak, so a broken encoder cannot hide behind successful collects. It
also disables itself when one window's failure rate (failed attempts / all
attempts, from ``collector.collect_failed_sample()``) exceeds
``failure_rate_threshold`` (provisionally 0.2, a constructor parameter and not a
user option), which is what a run of intermittent failures looks like when it
never reaches three in a row. Windows with fewer than
``min_failure_rate_attempts`` attempts (100, one second at 100 Hz) are not
judged: one failure in a handful of attempts says nothing. The rate is exposed on
:class:`SamplingWindow` and by :meth:`Sampler.last_failure_rate`. Nothing is
ever raised into application code.

A :class:`Sampler` is meant to be driven from one thread (the window loop):
``recheck_gil``, ``collect_window`` and ``encode_window`` are not synchronised.

The window API is deliberately blocking: :meth:`Sampler.collect_window` runs one
``sample()`` call for ``duration_sec`` and returns the aggregated stacks with
their timestamps. The 60 s window loop, the public switch, ``shutdown`` and
``os.register_at_fork`` belong to the client lifecycle, not here.
"""

from __future__ import annotations

import importlib
import inspect
import os
import sys
import sysconfig
import threading
import time
from dataclasses import dataclass, field
from typing import Any, Callable, Dict, Iterable, Mapping, Optional, Sequence, Tuple

from ._pprof import Frame, Stack, encode_cpu_profile
from .delivery import DeliveryWarn

#: First CPython with ``profiling.sampling``.
MIN_PYTHON = (3, 15)
#: Sampling frequency and the derived interval/period (100 Hz).
SAMPLING_RATE_HZ = 100
SAMPLE_INTERVAL_USEC = 1_000_000 // SAMPLING_RATE_HZ
SAMPLE_PERIOD_NS = 1_000_000_000 // SAMPLING_RATE_HZ
#: One upload window samples ``WINDOW_SECONDS`` every ``WINDOW_INTERVAL_SECONDS``
#: (Go SDK: ``profilingCPUWindow`` and the default ``ProfilingInterval``); the
#: interval is the window loop's business, it is only recorded here.
WINDOW_SECONDS = 10.0
WINDOW_INTERVAL_SECONDS = 60.0
#: The ``type`` sent to the ingest API.
PROFILE_TYPE = "cpu"
#: Consecutive failures before the sampler disables itself (owner decision).
MAX_CONSECUTIVE_FAILURES = 3
#: A window whose failed attempts exceed this share of all its attempts disables the
#: sampler. Provisional: the owner has not confirmed the value; settle it before S3.
#: A parameter of :class:`Sampler`, deliberately not a user-facing option.
FAILURE_RATE_THRESHOLD = 0.2
#: Windows with fewer attempts than this are not judged by their failure rate
#: (a window cut short by shutdown, say). One second of attempts at 100 Hz.
MIN_FAILURE_RATE_ATTEMPTS = 100
#: Suggested delay before the caller retries a failed window (doubles per failure).
RETRY_BACKOFF_INITIAL = 1.0
RETRY_BACKOFF_MAX = 30.0
#: Length of the trial sample run by the feature probe.
TRIAL_SAMPLE_SECONDS = 0.1

#: Probe outcomes; stable strings so callers can log or assert on them.
REASON_PYTHON_TOO_OLD = "python<3.15"
REASON_PLATFORM = "not-linux"
REASON_GIL_BUILD = "gil-build"
REASON_GIL_ENABLED = "gil-enabled"
REASON_IMPORT_FAILED = "import-failed"
REASON_SIGNATURE_MISMATCH = "signature-mismatch"
REASON_MODE_MISSING = "mode-constant-missing"
REASON_TRIAL_FAILED = "trial-sample-failed"
REASON_DISABLED = "too-many-failures"
REASON_FAILURE_RATE = "failure-rate-exceeded"

_SAMPLE_MODULE = "profiling.sampling.sample"
_CONSTANTS_MODULE = "profiling.sampling.constants"
_REQUIRED_SAMPLE_PARAMS = ("pid", "collector", "all_threads", "mode", "duration_sec")


@dataclass(frozen=True)
class SamplerProbe:
    """Result of the feature probe.

    ``available`` false always carries a ``reason`` from the ``REASON_*`` set and
    a human-readable ``detail``; nothing is raised for an unsupported
    interpreter.
    """

    available: bool
    reason: Optional[str] = None
    detail: str = ""
    mode: Optional[int] = None
    sample: Optional[Callable[..., Any]] = field(default=None, repr=False, compare=False)
    module: Any = field(default=None, repr=False, compare=False)


@dataclass(frozen=True)
class SamplingWindow:
    """One sampled window: aggregated leaf-first stacks plus its time span."""

    counts: Mapping[Stack, int]
    start_ns: int
    duration_ns: int
    samples: int = 0
    failed_samples: int = 0
    #: Every attempt the profiler made: stacks it read (including empty ones and the
    #: sampling thread's own) plus ``failed_samples``.
    attempts: int = 0

    @property
    def end_ns(self) -> int:
        return self.start_ns + self.duration_ns

    @property
    def failure_rate(self) -> float:
        return self.failed_samples / self.attempts if self.attempts > 0 else 0.0


def gil_enabled(sys_module: Any = None) -> bool:
    """Whether the GIL is on right now; true when that cannot be told.

    ``sys._is_gil_enabled`` exists from 3.13. A missing attribute or one that
    raises counts as "enabled", the conservative answer: nothing samples then.
    """
    probe = getattr(sys_module if sys_module is not None else sys, "_is_gil_enabled", None)
    if not callable(probe):
        return True
    try:
        return bool(probe())
    except Exception:
        return True


def _gil_enabled_now() -> bool:
    return gil_enabled()


def _runtime_gate(
    sys_module: Any, sysconfig_module: Any
) -> Optional[Tuple[str, str]]:
    """First failing platform/build/GIL gate as ``(reason, detail)``, else ``None``.

    On a GIL build the trial sample does not raise, it just returns nothing, so
    these checks must run before the trial (and before the private import).
    """
    platform = getattr(sys_module, "platform", "")
    if platform != "linux":
        return REASON_PLATFORM, f"in-process sampling is Linux-only, not {platform!r}"
    try:
        free_threaded = bool(sysconfig_module.get_config_var("Py_GIL_DISABLED"))
    except Exception:
        free_threaded = False
    if not free_threaded:
        return REASON_GIL_BUILD, "this CPython is not a free-threaded (Py_GIL_DISABLED) build"
    if gil_enabled(sys_module):
        detail = "the GIL is enabled at runtime"
        if not callable(getattr(sys_module, "_is_gil_enabled", None)):
            detail = "sys._is_gil_enabled is unavailable, so the GIL state is unknown"
        return REASON_GIL_ENABLED, detail
    return None


def probe_sampler(
    *,
    importer: Optional[Callable[[str], Any]] = None,
    version_info: Optional[Sequence[int]] = None,
    trial_seconds: float = TRIAL_SAMPLE_SECONDS,
    sys_module: Any = None,
    sysconfig_module: Any = None,
) -> SamplerProbe:
    """Decide whether this interpreter can be sampled in-process.

    ``importer``, ``version_info``, ``trial_seconds``, ``sys_module`` and
    ``sysconfig_module`` are seams for tests; production callers pass nothing.
    """
    version = tuple(version_info if version_info is not None else sys.version_info)
    if version[:2] < MIN_PYTHON:
        return SamplerProbe(
            False,
            reason=REASON_PYTHON_TOO_OLD,
            detail=f"Python {version[0]}.{version[1]} has no profiling.sampling",
        )

    refused = _runtime_gate(
        sys_module if sys_module is not None else sys,
        sysconfig_module if sysconfig_module is not None else sysconfig,
    )
    if refused is not None:
        return SamplerProbe(False, reason=refused[0], detail=refused[1])

    import_module = importer or importlib.import_module
    try:
        sample_module = import_module(_SAMPLE_MODULE)
        constants = import_module(_CONSTANTS_MODULE)
    except Exception as exc:  # ImportError, but stay defensive about bad modules
        return SamplerProbe(False, reason=REASON_IMPORT_FAILED, detail=repr(exc))

    sample_fn = getattr(sample_module, "sample", None)
    if not callable(sample_fn):
        return SamplerProbe(
            False, reason=REASON_SIGNATURE_MISMATCH, detail="sample.sample is missing or not callable"
        )
    try:
        parameters = inspect.signature(sample_fn).parameters
    except (TypeError, ValueError) as exc:
        return SamplerProbe(False, reason=REASON_SIGNATURE_MISMATCH, detail=repr(exc))
    missing = [name for name in _REQUIRED_SAMPLE_PARAMS if name not in parameters]
    if missing:
        return SamplerProbe(
            False,
            reason=REASON_SIGNATURE_MISMATCH,
            detail="sample.sample is missing parameters: " + ", ".join(missing),
        )

    mode = getattr(constants, "PROFILING_MODE_CPU", None)
    if not isinstance(mode, int) or isinstance(mode, bool):
        return SamplerProbe(
            False,
            reason=REASON_MODE_MISSING,
            detail="constants.PROFILING_MODE_CPU is missing",
        )

    _silence_stats_output(sample_module)
    try:
        sample_fn(
            os.getpid(),
            _TrialCollector(SAMPLE_INTERVAL_USEC),
            duration_sec=trial_seconds,
            all_threads=True,
            mode=mode,
        )
    except KeyboardInterrupt:  # pragma: no cover - the probe runs on a quiet thread
        raise
    except BaseException as exc:  # sample() raises SystemExit when it may not sample
        return SamplerProbe(False, reason=REASON_TRIAL_FAILED, detail=repr(exc))

    return SamplerProbe(True, mode=mode, sample=sample_fn, module=sample_module)


class Sampler:
    """Window-at-a-time CPU sampler for the current process.

    ``Sampler(probe_sampler())`` never samples; :class:`SamplerProbe` decides.
    :meth:`collect_window` blocks for the requested duration and returns
    :class:`SamplingWindow` or ``None`` when the window could not be collected
    (the sampler then counts a failure and may disable itself).

    Three things stop a sampler for good (:meth:`disabled`, with a stable
    :meth:`reason`): ``max_consecutive_failures`` windows in a row that raised or
    whose encoding failed (only a successful :meth:`encode_window` resets the
    streak); one window with at least ``min_failure_rate_attempts`` attempts whose
    share of failed attempts exceeds ``failure_rate_threshold`` (that window is
    discarded); and
    :meth:`recheck_gil` finding the GIL switched on again.
    """

    def __init__(
        self,
        probe: SamplerProbe,
        *,
        warn: Optional[DeliveryWarn] = None,
        rate_hz: int = SAMPLING_RATE_HZ,
        max_consecutive_failures: int = MAX_CONSECUTIVE_FAILURES,
        failure_rate_threshold: float = FAILURE_RATE_THRESHOLD,
        min_failure_rate_attempts: int = MIN_FAILURE_RATE_ATTEMPTS,
        encoder: Optional[Callable[..., bytes]] = None,
        gil_enabled: Optional[Callable[[], bool]] = None,
    ) -> None:
        if rate_hz <= 0:
            raise ValueError("rate_hz must be positive")
        if not 0.0 <= failure_rate_threshold <= 1.0:
            raise ValueError("failure_rate_threshold must be within [0, 1]")
        self._probe = probe
        self._warn = warn
        self._rate_hz = rate_hz
        self._interval_usec = max(1, 1_000_000 // rate_hz)
        self._period_ns = max(1, 1_000_000_000 // rate_hz)
        self._max_failures = max(1, max_consecutive_failures)
        self._failure_rate_threshold = failure_rate_threshold
        self._min_rate_attempts = max(0, min_failure_rate_attempts)
        self._encoder = encoder or encode_cpu_profile
        self._gil_enabled = gil_enabled or _gil_enabled_now
        self._failures = 0
        self._stopped: Optional[str] = None  # REASON_* once disabled
        self._stopped_detail = ""
        self._last_error: Optional[BaseException] = None
        self._last_failure_rate = 0.0

    @classmethod
    def create(
        cls,
        *,
        warn: Optional[DeliveryWarn] = None,
        rate_hz: int = SAMPLING_RATE_HZ,
        max_consecutive_failures: int = MAX_CONSECUTIVE_FAILURES,
        failure_rate_threshold: float = FAILURE_RATE_THRESHOLD,
        min_failure_rate_attempts: int = MIN_FAILURE_RATE_ATTEMPTS,
    ) -> "Sampler":
        """Probe this interpreter and return a sampler (possibly unavailable)."""
        return cls(
            probe_sampler(),
            warn=warn,
            rate_hz=rate_hz,
            max_consecutive_failures=max_consecutive_failures,
            failure_rate_threshold=failure_rate_threshold,
            min_failure_rate_attempts=min_failure_rate_attempts,
        )

    # --- state -------------------------------------------------------------

    def available(self) -> bool:
        return self._probe.available and self._stopped is None

    def reason(self) -> Optional[str]:
        if self._stopped is not None:
            return self._stopped
        return self._probe.reason

    def detail(self) -> str:
        if self._stopped is not None:
            return self._stopped_detail
        return self._probe.detail

    def disabled(self) -> bool:
        return self._stopped is not None

    def consecutive_failures(self) -> int:
        return self._failures

    def last_failure_rate(self) -> float:
        """Share of failed attempts in the latest completed window (0.0 before any)."""
        return self._last_failure_rate

    @property
    def failure_rate_threshold(self) -> float:
        return self._failure_rate_threshold

    def recheck_gil(self) -> bool:
        """Re-read the GIL state; ``True`` while sampling is still allowed.

        The window loop calls this before every window: a C extension imported
        later can switch the GIL back on, after which sampling silently returns
        nothing. Only the GIL state is looked at. If the GIL is on the sampler
        stops for good (reason :data:`REASON_GIL_ENABLED`, no flapping back) and
        the caller reports it; the sampler emits no warning of its own here.
        """
        if not self.available():
            return False
        if self._gil_enabled():
            self._stopped = REASON_GIL_ENABLED
            self._stopped_detail = "the GIL was enabled after sampling started"
            return False
        return True

    def backoff_seconds(
        self, *, initial: float = RETRY_BACKOFF_INITIAL, maximum: float = RETRY_BACKOFF_MAX
    ) -> float:
        """Delay the window loop should wait before retrying a failed window."""
        if self._failures <= 0:
            return 0.0
        return min(maximum, initial * (2 ** (self._failures - 1)))

    @property
    def rate_hz(self) -> int:
        return self._rate_hz

    @property
    def interval_usec(self) -> int:
        return self._interval_usec

    @property
    def period_ns(self) -> int:
        return self._period_ns

    @property
    def mode(self) -> Optional[int]:
        return self._probe.mode

    # --- sampling ----------------------------------------------------------

    def collect_window(self, duration_sec: float = WINDOW_SECONDS) -> Optional[SamplingWindow]:
        """Sample ``duration_sec`` seconds and return the aggregated stacks.

        Blocking: one ``sample()`` call for the whole duration, in the calling
        thread, which is therefore the sampling thread and is excluded from the
        window by its native id.
        """
        if duration_sec <= 0:
            raise ValueError("duration_sec must be positive")
        if not self.available():
            return None

        collector = _AggregatingCollector(
            self._interval_usec, exclude_thread_ids={threading.get_native_id()}
        )
        start_ns = time.time_ns()
        started = time.monotonic_ns()
        try:
            self._probe.sample(
                os.getpid(),
                collector,
                duration_sec=duration_sec,
                all_threads=True,
                mode=self._probe.mode,
            )
        except KeyboardInterrupt:  # pragma: no cover - the sampler thread owns it
            raise
        except BaseException as exc:  # SystemExit on permission errors, plus bugs
            self._record_failure("profiling sample failed", exc)
            return None
        duration_ns = max(0, time.monotonic_ns() - started)

        window = SamplingWindow(
            counts=dict(collector.counts),
            start_ns=start_ns,
            duration_ns=duration_ns,
            samples=collector.samples,
            failed_samples=collector.failed_samples,
            attempts=collector.attempts + collector.failed_samples,
        )
        self._last_failure_rate = window.failure_rate
        if (
            window.attempts >= self._min_rate_attempts
            and window.failure_rate > self._failure_rate_threshold
        ):
            self._disable_for_failure_rate(window)
            return None
        return window

    def encode_window(self, window: Optional[SamplingWindow]) -> Optional[bytes]:
        """Encode a window as gzip pprof bytes, or ``None`` if the encoder raises."""
        if window is None:
            return None
        try:
            data = self._encoder(
                window.counts,
                period_ns=self._period_ns,
                start_ns=window.start_ns,
                duration_ns=window.duration_ns,
            )
        except (TypeError, ValueError) as exc:
            self._record_failure("profiling encode failed", exc)
            return None
        self._failures = 0
        self._last_error = None
        return data

    # --- failures ----------------------------------------------------------

    def _record_failure(self, reason: str, exc: BaseException) -> None:
        if self._stopped is not None:
            return
        self._failures += 1
        self._last_error = exc
        if self._failures >= self._max_failures:
            self._stopped = REASON_DISABLED
            self._stopped_detail = repr(exc)
            self._emit_warn(
                f"{reason}; profiling disabled after {self._failures} consecutive failures", exc
            )
            return
        self._emit_warn(reason, exc)

    def _disable_for_failure_rate(self, window: SamplingWindow) -> None:
        detail = (
            f"failure rate {window.failure_rate:.1%} "
            f"({window.failed_samples} of {window.attempts} attempts) "
            f"exceeds the {self._failure_rate_threshold:.1%} threshold"
        )
        self._stopped = REASON_FAILURE_RATE
        self._stopped_detail = detail
        self._emit_warn(f"profiling disabled: {detail}", None)

    def _emit_warn(self, reason: str, exc: Optional[BaseException]) -> None:
        if self._warn is None:
            return
        try:
            self._warn(reason, exc, self._failures)
        except Exception:  # a broken warn callback must not break the sampler
            return


class _AggregatingCollector:
    """Duck-typed ``profiling.sampling.Collector`` that aggregates stacks.

    ``sample()`` only needs ``sample_interval_usec``, ``aggregating``,
    ``collect()`` and ``collect_failed_sample()``, so this deliberately does not
    subclass ``profiling.sampling.Collector``: the probe may hand us a module we
    must not import at module import time.
    """

    aggregating = True

    def __init__(self, interval_usec: int, *, exclude_thread_ids: Iterable[int] = ()) -> None:
        self.sample_interval_usec = interval_usec
        self._exclude = set(exclude_thread_ids)
        self.counts: Dict[Stack, int] = {}
        self.samples = 0
        self.failed_samples = 0
        #: Successful attempts, whatever they contained (``len(timestamps_us)`` per call).
        self.attempts = 0

    def collect(self, stack_frames: Any, timestamps_us: Any = None) -> None:
        weight = len(timestamps_us) if timestamps_us else 1
        self.attempts += weight
        for frames, thread_id in _iter_stacks(stack_frames):
            if thread_id is not None and thread_id in self._exclude:
                continue
            stack = _stack_of(frames)
            if not stack:
                continue
            self.counts[stack] = self.counts.get(stack, 0) + weight
            self.samples += weight

    def collect_failed_sample(self) -> None:
        self.failed_samples += 1

    def export(self, filename: str) -> bool:
        raise NotImplementedError("lazy-observe aggregates samples in memory")


class _TrialCollector:
    """Throwaway collector for the probe's trial sample."""

    aggregating = False

    def __init__(self, interval_usec: int) -> None:
        self.sample_interval_usec = interval_usec

    def collect(self, stack_frames: Any, timestamps_us: Any = None) -> None:
        return None

    def collect_failed_sample(self) -> None:
        return None

    def export(self, filename: str) -> bool:
        raise NotImplementedError("lazy-observe never exports trial samples")


def _iter_stacks(stack_frames: Any) -> Iterable[Tuple[Any, Optional[int]]]:
    """Yield ``(frames, thread_id)`` from the unwinder's interpreter snapshots."""
    for interpreter in stack_frames or ():
        for thread in getattr(interpreter, "threads", None) or ():
            frames = getattr(thread, "frame_info", None)
            if frames:
                yield frames, getattr(thread, "thread_id", None)


def _stack_of(frames: Iterable[Any]) -> Stack:
    """Convert unwinder frames (already leaf-first) into ``_pprof`` frames."""
    stack = []
    for frame in frames:
        name, filename, line = _frame_parts(frame)
        if not name:
            continue
        stack.append((name, filename, line))
    return tuple(stack)


def _frame_parts(frame: Any) -> Frame:
    if isinstance(frame, tuple):
        filename = frame[0] if len(frame) > 0 else ""
        location = frame[1] if len(frame) > 1 else None
        name = frame[2] if len(frame) > 2 else ""
    else:
        filename = getattr(frame, "filename", "")
        location = getattr(frame, "location", None)
        name = getattr(frame, "funcname", "")
    return _text(name), _text(filename), _lineno(location)


def _text(value: Any) -> str:
    if value is None:
        return ""
    if isinstance(value, str):
        return value
    return str(value)


def _lineno(location: Any) -> int:
    if location is None:
        return 0
    if isinstance(location, bool):
        return 0
    if isinstance(location, int):
        return max(0, location)
    try:
        return max(0, int(location[0]))
    except (IndexError, KeyError, TypeError, ValueError):
        return 0


def _silence_stats_output(sample_module: Any) -> None:
    """Drop ``SampleProfiler.sample``'s stdout statistics.

    3.15.2 prints "Captured N samples ...", the sample rate, the error rate and
    missed-sample warnings with the plain ``print`` builtin. Shadowing ``print``
    in that module's namespace silences it without touching ``sys.stdout`` (which
    would swallow the application's own output for the whole window). The
    assignment is idempotent.
    """
    try:
        sample_module.print = _discard_output
    except Exception:  # pragma: no cover - module objects are always writable
        return


def _discard_output(*args: Any, **kwargs: Any) -> None:
    return None
