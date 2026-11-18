"""Structural tests for the dependency-free pprof encoder.

The decoder below is deliberately minimal: it only understands the protobuf
wire types the encoder emits. The independent read-back of the same bytes is
done by the Go test in ``internal/gateway/python_pprof_test.go``.
"""

from __future__ import annotations

import gzip

import pytest

from lazyobserve._pprof import encode_cpu_profile
from tests.pprof_fixture import FIXTURE_PATH, build_fixture

PERIOD_NS = 10_000_000
START_NS = 1_700_000_000_000_000_000
DURATION_NS = 60_000_000_000


def _read_varint(data: bytes, pos: int):
    result = 0
    shift = 0
    while True:
        byte = data[pos]
        pos += 1
        result |= (byte & 0x7F) << shift
        if not byte & 0x80:
            return result, pos
        shift += 7


def _parse_fields(data: bytes):
    fields = {}
    pos = 0
    while pos < len(data):
        key, pos = _read_varint(data, pos)
        number, wire = key >> 3, key & 0x7
        if wire == 0:
            value, pos = _read_varint(data, pos)
        elif wire == 2:
            length, pos = _read_varint(data, pos)
            value = data[pos : pos + length]
            pos += length
        elif wire == 1:
            value = data[pos : pos + 8]
            pos += 8
        elif wire == 5:
            value = data[pos : pos + 4]
            pos += 4
        else:
            raise AssertionError(f"unsupported wire type {wire}")
        fields.setdefault(number, []).append(value)
    return fields


def _packed_ints(fields, number):
    values = []
    for payload in fields.get(number, []):
        pos = 0
        while pos < len(payload):
            value, pos = _read_varint(payload, pos)
            values.append(value)
    return values


def _one(fields, number):
    return fields[number][0]


def _string_table(profile):
    return [value.decode("utf-8", "surrogatepass") for value in profile[6]]


def _value_type(message, strings):
    fields = _parse_fields(message)
    return strings[_one(fields, 1)], strings[_one(fields, 2)]


def _sample_type(profile):
    strings = _string_table(profile)
    return [_value_type(message, strings) for message in profile[1]]


def _functions(profile):
    strings = _string_table(profile)
    functions = {}
    for message in profile.get(5, []):
        fields = _parse_fields(message)
        functions[_one(fields, 1)] = (
            strings[_one(fields, 2)],
            strings[_one(fields, 4)],
        )
    return functions


def _locations(profile):
    functions = _functions(profile)
    locations = {}
    for message in profile.get(4, []):
        fields = _parse_fields(message)
        location_id = _one(fields, 1)
        line = _parse_fields(_one(fields, 4))
        locations[location_id] = (
            functions[_one(line, 1)][0],
            functions[_one(line, 1)][1],
            _one(line, 2),
        )
    return locations


def _samples(profile):
    locations = _locations(profile)
    samples = []
    for message in profile.get(2, []):
        fields = _parse_fields(message)
        ids = _packed_ints(fields, 1)
        values = _packed_ints(fields, 2)
        samples.append(([locations[location_id] for location_id in ids], values))
    return samples


def test_sample_types_and_period():
    data = encode_cpu_profile(
        {((("main", "/app/main.py", 10),)): 1},
        period_ns=PERIOD_NS,
        start_ns=START_NS,
        duration_ns=DURATION_NS,
    )
    profile = _parse_fields(gzip.decompress(data))
    assert _sample_type(profile) == [("samples", "count"), ("cpu", "nanoseconds")]
    period_type = _parse_fields(_one(profile, 11))
    strings = _string_table(profile)
    assert _value_type(_one(profile, 11), strings) == ("cpu", "nanoseconds")
    assert _one(period_type, 1) == strings.index("cpu")
    assert _one(profile, 12) == PERIOD_NS
    assert _one(profile, 9) == START_NS
    assert _one(profile, 10) == DURATION_NS


def test_string_table_starts_empty_and_deduplicates():
    data = encode_cpu_profile(
        {
            (
                ("hot_parse", "/app/parse.py", 77),
                ("handle", "/app/server.py", 42),
                ("main", "/app/main.py", 10),
            ): 3,
            (
                ("hot_render", "/app/render.py", 31),
                ("handle", "/app/server.py", 42),
                ("main", "/app/main.py", 10),
            ): 2,
        },
        period_ns=PERIOD_NS,
        start_ns=START_NS,
        duration_ns=DURATION_NS,
    )
    profile = _parse_fields(gzip.decompress(data))
    strings = _string_table(profile)
    assert strings[0] == ""
    assert strings.count("handle") == 1
    assert len(_functions(profile)) == 4
    assert len(_locations(profile)) == 4


def test_samples_are_leaf_first_with_counts_and_cpu_time():
    data = encode_cpu_profile(
        {
            (
                ("hot_parse", "/app/parse.py", 77),
                ("handle", "/app/server.py", 42),
                ("main", "/app/main.py", 10),
            ): 75,
            (
                ("hot_render", "/app/render.py", 31),
                ("handle", "/app/server.py", 42),
                ("main", "/app/main.py", 10),
            ): 25,
        },
        period_ns=PERIOD_NS,
        start_ns=START_NS,
        duration_ns=DURATION_NS,
    )
    samples = _samples(_parse_fields(gzip.decompress(data)))
    assert samples == [
        (
            [
                ("hot_parse", "/app/parse.py", 77),
                ("handle", "/app/server.py", 42),
                ("main", "/app/main.py", 10),
            ],
            [75, 75 * PERIOD_NS],
        ),
        (
            [
                ("hot_render", "/app/render.py", 31),
                ("handle", "/app/server.py", 42),
                ("main", "/app/main.py", 10),
            ],
            [25, 25 * PERIOD_NS],
        ),
    ]


def test_empty_profile_is_valid_and_has_no_samples():
    data = encode_cpu_profile(
        {}, period_ns=PERIOD_NS, start_ns=START_NS, duration_ns=DURATION_NS
    )
    profile = _parse_fields(gzip.decompress(data))
    assert _sample_type(profile) == [("samples", "count"), ("cpu", "nanoseconds")]
    assert 2 not in profile
    assert _string_table(profile)[0] == ""


def test_empty_stacks_and_non_positive_counts_are_skipped():
    data = encode_cpu_profile(
        {
            (): 4,
            ((("main", "/app/main.py", 10),)): 0,
            ((("render", "/app/render.py", 3),)): -2,
        },
        period_ns=PERIOD_NS,
        start_ns=START_NS,
        duration_ns=DURATION_NS,
    )
    assert 2 not in _parse_fields(gzip.decompress(data))


def test_encoding_is_deterministic():
    stacks = {
        (("hot_parse", "/app/parse.py", 77), ("main", "/app/main.py", 10)): 7,
    }
    first = encode_cpu_profile(
        stacks, period_ns=PERIOD_NS, start_ns=START_NS, duration_ns=DURATION_NS
    )
    second = encode_cpu_profile(
        stacks, period_ns=PERIOD_NS, start_ns=START_NS, duration_ns=DURATION_NS
    )
    assert first == second
    assert first[:2] == b"\x1f\x8b"
    # gzip MTIME field must stay zero so fixtures are byte-stable.
    assert first[4:8] == b"\x00\x00\x00\x00"


def test_non_integer_line_numbers_are_rejected():
    with pytest.raises((TypeError, ValueError)):
        encode_cpu_profile(
            {((("main", "/app/main.py", "ten"),)): 1},
            period_ns=PERIOD_NS,
            start_ns=START_NS,
            duration_ns=DURATION_NS,
        )


def test_committed_fixture_matches_encoder():
    assert FIXTURE_PATH.exists(), (
        "missing pprof fixture; regenerate with "
        "`cd sdk/python && .venv/bin/python -m tests.generate_pprof_fixture`"
    )
    assert gzip.decompress(FIXTURE_PATH.read_bytes()) == gzip.decompress(build_fixture())
