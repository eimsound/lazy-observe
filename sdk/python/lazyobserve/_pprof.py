"""Zero-dependency encoder for the pprof format used by ``POST /api/profiles``.

Input is a mapping of leaf-first stacks to sample counts. Each frame is a
``(function_name, filename, line_number)`` tuple. The encoder writes the subset
of ``profile.proto`` the platform consumes:

* sample types ``samples/count`` and ``cpu/nanoseconds``;
* nested ``period_type`` (``cpu/nanoseconds``) with ``period = 1e9 / rate``;
* a string table whose first entry is the empty string;
* deduplicated ``Function`` and ``Location`` messages.

The output is gzip-compressed and byte-stable (gzip MTIME is zero) so it can be
committed as a fixture and read back by the Go ``pprof`` parser.
"""

from __future__ import annotations

import gzip
import io
from typing import Dict, List, Mapping, Sequence, Tuple

Frame = Tuple[str, str, int]
Stack = Tuple[Frame, ...]

_SAMPLE_TYPE_TYPE = "samples"
_SAMPLE_TYPE_UNIT = "count"
_CPU_TYPE = "cpu"
_CPU_UNIT = "nanoseconds"


def encode_cpu_profile(
    stacks: Mapping[Stack, int],
    *,
    period_ns: int,
    start_ns: int,
    duration_ns: int,
) -> bytes:
    """Encode aggregated stacks as a gzip-compressed pprof profile.

    ``stacks`` is ordered leaf-first (``pprof`` sample location order). Empty
    stacks and non-positive counts are skipped. The returned bytes are
    deterministic for identical inputs.
    """
    if period_ns <= 0:
        raise ValueError("period_ns must be positive")
    if start_ns < 0 or duration_ns < 0:
        raise ValueError("start_ns and duration_ns cannot be negative")

    strings = _StringTable()
    sample_counts = _value_type(strings, _SAMPLE_TYPE_TYPE, _SAMPLE_TYPE_UNIT)
    cpu_type = _value_type(strings, _CPU_TYPE, _CPU_UNIT)

    functions: Dict[Tuple[str, str], int] = {}
    locations: Dict[Frame, int] = {}
    function_messages: List[bytes] = []
    location_messages: List[bytes] = []
    sample_messages: List[bytes] = []

    for stack, count in _ordered_stacks(stacks):
        if not stack or count <= 0:
            continue
        location_ids = [
            _location_id(frame, strings, functions, locations, function_messages, location_messages)
            for frame in stack
        ]
        sample_messages.append(
            _packed(1, location_ids) + _packed(2, [count, count * period_ns])
        )

    body = bytearray()
    body += _bytes(1, sample_counts) + _bytes(1, cpu_type)
    for message in sample_messages:
        body += _bytes(2, message)
    for message in location_messages:
        body += _bytes(4, message)
    for message in function_messages:
        body += _bytes(5, message)
    for value in strings.items:
        body += _bytes(6, value.encode("utf-8", "surrogatepass"))
    body += _uint(9, start_ns)
    body += _uint(10, duration_ns)
    body += _bytes(11, cpu_type)
    body += _uint(12, period_ns)
    body += _uint(14, strings(_CPU_TYPE))
    return _gzip(bytes(body))


def _ordered_stacks(stacks: Mapping[Stack, int]):
    # Sorting keeps fixtures stable even when callers build the mapping in a
    # different insertion order.
    try:
        return sorted(stacks.items(), key=lambda item: item[0])
    except TypeError:
        return list(stacks.items())


def _location_id(
    frame: Frame,
    strings: "_StringTable",
    functions: Dict[Tuple[str, str], int],
    locations: Dict[Frame, int],
    function_messages: List[bytes],
    location_messages: List[bytes],
) -> int:
    name, filename, line = _frame_parts(frame)
    location_id = locations.get((name, filename, line))
    if location_id is not None:
        return location_id

    function_key = (name, filename)
    function_id = functions.get(function_key)
    if function_id is None:
        function_id = len(functions) + 1
        functions[function_key] = function_id
        name_index = strings(name)
        function_messages.append(
            _uint(1, function_id)
            + _uint(2, name_index)
            + _uint(3, name_index)
            + _uint(4, strings(filename))
        )

    location_id = len(locations) + 1
    locations[(name, filename, line)] = location_id
    line_message = _uint(1, function_id) + _uint(2, line)
    location_messages.append(
        _uint(1, location_id) + _bytes(4, line_message)
    )
    return location_id


def _frame_parts(frame: Frame) -> Tuple[str, str, int]:
    if not isinstance(frame, tuple) or len(frame) != 3:
        raise TypeError("frame must be a (function, filename, line) tuple")
    name, filename, line = frame
    if not isinstance(name, str) or not isinstance(filename, str):
        raise TypeError("frame function and filename must be strings")
    if isinstance(line, bool) or not isinstance(line, int):
        raise TypeError("frame line must be an integer")
    return name, filename, max(0, line)


class _StringTable:
    def __init__(self) -> None:
        self.items: List[str] = [""]
        self.index: Dict[str, int] = {"": 0}

    def __call__(self, value: str) -> int:
        existing = self.index.get(value)
        if existing is not None:
            return existing
        index = len(self.items)
        self.items.append(value)
        self.index[value] = index
        return index


def _value_type(strings: "_StringTable", name: str, unit: str) -> bytes:
    return _uint(1, strings(name)) + _uint(2, strings(unit))


def _varint(value: int) -> bytes:
    if value < 0:
        value &= (1 << 64) - 1
    out = bytearray()
    while True:
        bits = value & 0x7F
        value >>= 7
        if value:
            out.append(bits | 0x80)
        else:
            out.append(bits)
            return bytes(out)


def _key(field: int, wire: int) -> bytes:
    return _varint((field << 3) | wire)


def _uint(field: int, value: int) -> bytes:
    if not value:
        return b""
    return _key(field, 0) + _varint(value)


def _bytes(field: int, payload: bytes) -> bytes:
    return _key(field, 2) + _varint(len(payload)) + payload


def _packed(field: int, values: Sequence[int]) -> bytes:
    if not values:
        return b""
    return _bytes(field, b"".join(_varint(value) for value in values))


def _gzip(data: bytes) -> bytes:
    buffer = io.BytesIO()
    with gzip.GzipFile(fileobj=buffer, mode="wb", mtime=0) as writer:
        writer.write(data)
    return buffer.getvalue()
