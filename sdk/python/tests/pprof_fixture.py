"""Deterministic pprof fixture shared by the pytest drift check and the Go
read-back test in ``internal/gateway/python_pprof_test.go``.

Regenerate with::

    cd sdk/python && .venv/bin/python -m tests.generate_pprof_fixture
"""

from __future__ import annotations

from pathlib import Path

from lazy_observe._pprof import encode_cpu_profile

PERIOD_NS = 10_000_000  # 100 Hz
START_NS = 1_700_000_000_000_000_000
DURATION_NS = 60_000_000_000

STACKS = {
    (
        ("hot_parse", "/app/parse.py", 77),
        ("handle_request", "/app/server.py", 42),
        ("main", "/app/main.py", 10),
    ): 75,
    (
        ("hot_render", "/app/render.py", 31),
        ("handle_request", "/app/server.py", 42),
        ("main", "/app/main.py", 10),
    ): 25,
}

REPO_ROOT = Path(__file__).resolve().parents[3]
FIXTURE_PATH = REPO_ROOT / "internal/gateway/testdata/python_profiling/cpu.pprof.gz"


def build_fixture() -> bytes:
    return encode_cpu_profile(
        STACKS,
        period_ns=PERIOD_NS,
        start_ns=START_NS,
        duration_ns=DURATION_NS,
    )


def write_fixture() -> Path:
    FIXTURE_PATH.parent.mkdir(parents=True, exist_ok=True)
    FIXTURE_PATH.write_bytes(build_fixture())
    return FIXTURE_PATH
