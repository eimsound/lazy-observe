"""Shared GenAI semantic conformance tests backed by sdk/testdata/genai/."""

from __future__ import annotations

import asyncio
import json
from pathlib import Path
import re
import typing as t

import httpx
import pytest
from opentelemetry.trace import SpanKind, StatusCode

from lazy_observe.llm import (
    GEN_AI_ATTRS,
    LLM_BODY_INSPECTION_LIMIT,
    LLM_CONTENT_LIMIT,
    detect_llm_system,
)
from tests.helpers import last_span

FIXTURE_DIR = Path(__file__).resolve().parents[2] / "testdata" / "genai"
_REPEAT_RE = re.compile(r"\{\{repeat:([^:}]+):(\d+)\}\}")


def _load_fixture(name: str) -> dict:
    return json.loads((FIXTURE_DIR / name).read_text(encoding="utf-8"))


def _expand(text: str) -> str:
    return _REPEAT_RE.sub(lambda m: m.group(1) * int(m.group(2)), text)


CONTRACT = _load_fixture("contract.json")
CASES_SUITE = _load_fixture("cases.json")


def test_genai_contract_constants_and_limits():
    assert LLM_CONTENT_LIMIT == CONTRACT["content_limit_bytes"]
    assert LLM_BODY_INSPECTION_LIMIT == CONTRACT["body_inspection_limit_bytes"]
    assert GEN_AI_ATTRS == CONTRACT["attributes"]


@pytest.mark.parametrize("case", CONTRACT["path_detection_cases"], ids=lambda c: c["path"])
def test_genai_contract_path_detection(case: dict):
    got = detect_llm_system(case["path"]) or ""
    assert got == case["system"]


class _ChunkedSyncStream(httpx.SyncByteStream):
    def __init__(self, chunks: t.List[bytes]) -> None:
        self._chunks = chunks

    def __iter__(self) -> t.Iterator[bytes]:
        for chunk in self._chunks:
            yield chunk


class _ChunkedAsyncStream(httpx.AsyncByteStream):
    def __init__(self, chunks: t.List[bytes]) -> None:
        self._chunks = chunks

    async def __aiter__(self) -> t.AsyncIterator[bytes]:
        for chunk in self._chunks:
            yield chunk


def _assert_case_span(span, tc: dict) -> None:
    expect = tc["expect"]
    assert span is not None
    assert span.name == expect["span_name"]
    if expect["span_kind"] == "CLIENT":
        assert span.kind == SpanKind.CLIENT
    if expect["status"] == "ERROR":
        assert span.status.status_code == StatusCode.ERROR
    else:
        assert span.status.status_code != StatusCode.ERROR

    attrs = dict(span.attributes)
    for key, want in expect["attributes"].items():
        assert key in attrs, f"missing attribute {key}; attrs={attrs}"
        assert attrs[key] == want, f"attr[{key}]={attrs[key]!r} want={want!r}"
        assert type(attrs[key]) is type(want) or (
            isinstance(attrs[key], int) and not isinstance(attrs[key], bool) and isinstance(want, int)
        )
    for absent_key in expect.get("absent_attributes", []):
        assert absent_key not in attrs, f"unexpected attribute {absent_key}={attrs[absent_key]!r}"

    if expect.get("prompt_present"):
        prompt = attrs.get(GEN_AI_ATTRS["prompt"])
        assert isinstance(prompt, str) and prompt != ""
        assert "\ufffd" not in prompt
        encoded = prompt.encode("utf-8")
        assert encoded.decode("utf-8") == prompt
        max_bytes = expect.get("prompt_max_utf8_bytes", 0)
        if max_bytes:
            assert len(encoded) <= max_bytes
        contains = expect.get("prompt_contains", "")
        if contains:
            assert contains in prompt

    if "completion_exact" in expect and expect["completion_exact"] is not None:
        want_completion = _expand(expect["completion_exact"])
        completion = attrs.get(GEN_AI_ATTRS["completion"])
        assert isinstance(completion, str)
        assert "\ufffd" not in completion
        encoded = completion.encode("utf-8")
        assert encoded.decode("utf-8") == completion
        max_bytes = expect.get("completion_max_utf8_bytes", 0)
        if max_bytes:
            assert len(encoded) <= max_bytes
        assert completion == want_completion


@pytest.mark.parametrize("tc", CASES_SUITE["cases"], ids=lambda c: c["name"])
def test_genai_conformance_sync(client, tc: dict):
    client.config.capture_llm_content = bool(tc["capture_content"])
    resp_spec = tc["response"]
    headers = {"content-type": resp_spec["content_type"]}
    chunks = [_expand(c).encode("utf-8") for c in resp_spec.get("chunks", [])]

    def handler(_req: httpx.Request) -> httpx.Response:
        if chunks:
            return httpx.Response(resp_spec["status"], headers=headers, stream=_ChunkedSyncStream(chunks))
        return httpx.Response(
            resp_spec["status"],
            headers=headers,
            content=_expand(resp_spec.get("body", "")).encode("utf-8"),
        )

    transport = client.llm_transport(base=httpx.MockTransport(handler))
    req_body = _expand(json.dumps(tc["request"]["body"], ensure_ascii=False)).encode("utf-8")
    url = f"http://llm.conformance.test{tc['request']['path']}"

    with httpx.Client(transport=transport) as http:
        if chunks or not resp_spec["consume_body"]:
            with http.stream(
                tc["request"]["method"],
                url,
                headers={"content-type": "application/json"},
                content=req_body,
            ) as resp:
                if tc["expect"]["ended_before_consume"]:
                    assert last_span(client) is not None
                else:
                    assert last_span(client) is None
                if resp_spec["consume_body"]:
                    for _ in resp.iter_bytes():
                        pass
        else:
            resp = http.request(
                tc["request"]["method"],
                url,
                headers={"content-type": "application/json"},
                content=req_body,
            )
            assert resp.status_code == resp_spec["status"]

    _assert_case_span(last_span(client), tc)


@pytest.mark.parametrize("tc", CASES_SUITE["cases"], ids=lambda c: c["name"])
def test_genai_conformance_async(client, tc: dict):
    client.config.capture_llm_content = bool(tc["capture_content"])
    resp_spec = tc["response"]
    headers = {"content-type": resp_spec["content_type"]}
    chunks = [_expand(c).encode("utf-8") for c in resp_spec.get("chunks", [])]

    async def handler(_req: httpx.Request) -> httpx.Response:
        if chunks:
            return httpx.Response(resp_spec["status"], headers=headers, stream=_ChunkedAsyncStream(chunks))
        return httpx.Response(
            resp_spec["status"],
            headers=headers,
            content=_expand(resp_spec.get("body", "")).encode("utf-8"),
        )

    req_body = _expand(json.dumps(tc["request"]["body"], ensure_ascii=False)).encode("utf-8")
    url = f"http://llm.conformance.test{tc['request']['path']}"

    async def _run():
        transport = client.llm_transport(base=httpx.MockTransport(handler), client_kind="async")
        async with httpx.AsyncClient(transport=transport) as http:
            if chunks or not resp_spec["consume_body"]:
                async with http.stream(
                    tc["request"]["method"],
                    url,
                    headers={"content-type": "application/json"},
                    content=req_body,
                ) as resp:
                    if tc["expect"]["ended_before_consume"]:
                        assert last_span(client) is not None
                    else:
                        assert last_span(client) is None
                    if resp_spec["consume_body"]:
                        async for _ in resp.aiter_bytes():
                            pass
            else:
                resp = await http.request(
                    tc["request"]["method"],
                    url,
                    headers={"content-type": "application/json"},
                    content=req_body,
                )
                assert resp.status_code == resp_spec["status"]

    asyncio.run(_run())
    _assert_case_span(last_span(client), tc)
