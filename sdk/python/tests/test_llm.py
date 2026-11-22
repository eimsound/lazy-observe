import asyncio
import json

import httpx
import pytest
from opentelemetry.trace import StatusCode

from lazy_observe.llm import LLM_CONTENT_LIMIT, detect_llm_system
from tests.helpers import last_span


def _mock_openai(request):
    body = json.loads(request.content)
    assert body["model"] == "gpt-4o"
    return httpx.Response(
        200,
        json={
            "model": "gpt-4o",
            "usage": {"prompt_tokens": 10, "completion_tokens": 5},
            "choices": [{"message": {"content": "hello there"}}],
        },
    )


def test_openai_chat_span(client):
    client.config.capture_llm_content = True
    base = httpx.MockTransport(_mock_openai)
    transport = client.llm_transport(base=base)
    http = httpx.Client(transport=transport)
    resp = http.post(
        "http://llm.example/v1/chat/completions",
        json={
            "model": "gpt-4o",
            "messages": [{"role": "user", "content": "say hi"}],
        },
    )
    assert resp.status_code == 200
    span = last_span(client)
    assert span is not None
    assert span.name == "chat openai"
    assert span.kind.name == "CLIENT"
    attrs = dict(span.attributes)
    assert attrs["gen_ai.operation.name"] == "chat"
    assert attrs["gen_ai.provider.name"] == "openai"
    assert attrs["gen_ai.request.model"] == "gpt-4o"
    assert attrs["gen_ai.response.model"] == "gpt-4o"
    assert attrs["gen_ai.usage.input_tokens"] == 10
    assert attrs["gen_ai.usage.output_tokens"] == 5
    assert "say hi" in attrs["gen_ai.prompt"]
    assert attrs["gen_ai.completion"] == "hello there"


def test_non_llm_path_passthrough(client):
    assert detect_llm_system("/v1/messages/count_tokens") is None
    assert detect_llm_system("/api/chat/history") is None
    base = httpx.MockTransport(lambda request: httpx.Response(200, json={"ok": True}))
    transport = client.llm_transport(base=base)
    http = httpx.Client(transport=transport)
    resp = http.get("http://llm.example/health")
    assert resp.status_code == 200
    assert last_span(client) is None


def test_ollama_chat_usage(client):
    def handler(request):
        return httpx.Response(
            200,
            json={
                "model": "llama3",
                "prompt_eval_count": 7,
                "eval_count": 3,
                "message": {"content": "hello"},
            },
        )

    transport = client.llm_transport(base=httpx.MockTransport(handler))
    http = httpx.Client(transport=transport)
    http.post("http://llm.example/api/chat", json={"model": "llama3", "prompt": "hi"})
    span = last_span(client)
    assert span.name == "chat ollama"
    attrs = dict(span.attributes)
    assert attrs["gen_ai.usage.input_tokens"] == 7
    assert attrs["gen_ai.usage.output_tokens"] == 3


def test_zero_tokens_preserved_and_bool_rejected(client):
    def handler(request):
        return httpx.Response(
            200,
            json={
                "model": "gpt-4o",
                "usage": {"prompt_tokens": 0, "completion_tokens": 0},
                "choices": [{"message": {"content": ""}}],
            },
        )

    transport = client.llm_transport(base=httpx.MockTransport(handler))
    http = httpx.Client(transport=transport)
    http.post("http://llm.example/v1/chat/completions", json={"model": "gpt-4o"})
    attrs = dict(last_span(client).attributes)
    assert attrs["gen_ai.usage.input_tokens"] == 0
    assert attrs["gen_ai.usage.output_tokens"] == 0
    assert "gen_ai.usage.missing" not in attrs

    def bool_handler(request):
        return httpx.Response(
            200,
            json={
                "model": "gpt-4o",
                "usage": {"prompt_tokens": True, "completion_tokens": False},
            },
        )

    transport2 = client.llm_transport(base=httpx.MockTransport(bool_handler))
    httpx.Client(transport=transport2).post("http://llm.example/v1/chat/completions", json={"model": "gpt-4o"})
    attrs2 = dict(last_span(client).attributes)
    assert "gen_ai.usage.input_tokens" not in attrs2
    assert "gen_ai.usage.output_tokens" not in attrs2
    assert attrs2["gen_ai.usage.missing"] is True


def test_string_prompt_truncated_by_utf8_bytes(client):
    client.config.capture_llm_content = True
    # Each CJK character is 3 UTF-8 bytes; 2000 chars = 6000 bytes > 4096 bytes.
    big_prompt = "界" * 2000

    def handler(request):
        return httpx.Response(
            200,
            json={
                "model": "llama3",
                "prompt_eval_count": 1,
                "eval_count": 1,
                "message": {"content": big_prompt},
            },
        )

    http = httpx.Client(transport=client.llm_transport(base=httpx.MockTransport(handler)))
    http.post("http://llm.example/api/chat", json={"model": "llama3", "prompt": big_prompt})
    attrs = dict(last_span(client).attributes)
    for key in ("gen_ai.prompt", "gen_ai.completion"):
        val = attrs[key]
        assert len(val.encode("utf-8")) <= LLM_CONTENT_LIMIT
        assert val.encode("utf-8").decode("utf-8") == val


def test_error_status_on_http_4xx_and_exception_and_streaming_request_body(client):
    # 1. HTTP 429 on stream=True must set ERROR status and end span immediately.
    http_429 = httpx.Client(
        transport=client.llm_transport(
            base=httpx.MockTransport(lambda req: httpx.Response(429, json={"error": "rate limit"}))
        )
    )
    resp = http_429.post(
        "http://llm.example/v1/chat/completions",
        json={"model": "gpt-4o", "stream": True},
    )
    assert resp.status_code == 429
    span_429 = last_span(client)
    assert span_429 is not None
    assert span_429.status.status_code == StatusCode.ERROR

    # 2. Transport exception must record exception and ERROR status.
    def boom(req):
        raise httpx.ConnectError("upstream down")

    http_err = httpx.Client(transport=client.llm_transport(base=httpx.MockTransport(boom)))
    with pytest.raises(httpx.ConnectError):
        http_err.post("http://llm.example/v1/messages", json={"model": "claude-sonnet-4"})
    span_err = last_span(client)
    assert span_err.status.status_code == StatusCode.ERROR
    assert any(event.name == "exception" for event in span_err.events)

    # 3. Streaming request body must NOT raise httpx.RequestNotRead.
    def body_gen():
        yield b'{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'

    def ok_handler(req):
        req.read()
        return httpx.Response(
            200,
            json={"model": "gpt-4o", "usage": {"prompt_tokens": 5, "completion_tokens": 2}},
        )

    http_stream_req = httpx.Client(transport=client.llm_transport(base=httpx.MockTransport(ok_handler)))
    resp_stream_req = http_stream_req.post("http://llm.example/v1/chat/completions", content=body_gen())
    assert resp_stream_req.status_code == 200
    assert dict(last_span(client).attributes)["gen_ai.usage.input_tokens"] == 5


def test_sync_streaming_openai_and_anthropic_cache_usage(client):
    client.config.capture_llm_content = True
    sse_payload = (
        b'data: {"model":"gpt-4o","choices":[{"delta":{"content":"hel"}}]}\n\n'
        b'data: {"choices":[{"delta":{"content":"lo"}}],"usage":{"prompt_tokens":100,"completion_tokens":20,'
        b'"prompt_tokens_details":{"cached_tokens":60},"completion_tokens_details":{"reasoning_tokens":10}}}\n\n'
        b"data: [DONE]\n\n"
    )

    class _Stream(httpx.SyncByteStream):
        def __iter__(self):
            yield sse_payload[:40]
            yield sse_payload[40:]

    def handler(req):
        return httpx.Response(200, headers={"content-type": "text/event-stream"}, stream=_Stream())

    http = httpx.Client(transport=client.llm_transport(base=httpx.MockTransport(handler)))
    with http.stream(
        "POST",
        "http://llm.example/v1/chat/completions",
        json={"model": "gpt-4o", "stream": True, "messages": [{"role": "user", "content": "hi"}]},
    ) as resp:
        assert last_span(client) is None
        collected = b"".join(resp.iter_bytes())
    assert b"[DONE]" in collected
    attrs = dict(last_span(client).attributes)
    assert attrs["gen_ai.response.model"] == "gpt-4o"
    assert attrs["gen_ai.completion"] == "hello"
    assert attrs["gen_ai.usage.input_tokens"] == 100
    assert attrs["gen_ai.usage.output_tokens"] == 20
    assert attrs["gen_ai.usage.cache_read.input_tokens"] == 60
    assert attrs["gen_ai.usage.reasoning_tokens"] == 10
    assert "gen_ai.usage.missing" not in attrs


def test_async_streaming_anthropic_and_error_status(client):
    client.config.capture_llm_content = True
    sse_chunks = [
        b'event: message_start\ndata: {"type":"message_start","message":{"model":"claude-sonnet-4-20250514","usage":{"input_tokens":100,"cache_creation_input_tokens":5000,"cache_read_input_tokens":20000}}}\n\n',
        b'event: content_block_delta\ndata: {"type":"content_block_delta","delta":{"type":"text_delta","text":"async ok"}}\n\n',
        b'event: message_delta\ndata: {"type":"message_delta","usage":{"output_tokens":33}}\n\n',
    ]

    class _AsyncStream(httpx.AsyncByteStream):
        async def __aiter__(self):
            for chunk in sse_chunks:
                yield chunk

    async def _run():
        async def handler(req):
            if req.url.path == "/v1/messages":
                return httpx.Response(200, headers={"content-type": "text/event-stream"}, stream=_AsyncStream())
            return httpx.Response(500, json={"error": "boom"})

        async with httpx.AsyncClient(
            transport=client.llm_transport(base=httpx.MockTransport(handler), client_kind="async")
        ) as http:
            async with http.stream(
                "POST",
                "http://llm.example/v1/messages",
                json={"model": "claude-sonnet-4", "stream": True},
            ) as resp:
                assert last_span(client) is None
                async for _ in resp.aiter_bytes():
                    pass
            attrs = dict(last_span(client).attributes)
            assert attrs["gen_ai.response.model"] == "claude-sonnet-4-20250514"
            assert attrs["gen_ai.usage.input_tokens"] == 100
            assert attrs["gen_ai.usage.output_tokens"] == 33
            assert attrs["gen_ai.usage.cache_creation.input_tokens"] == 5000
            assert attrs["gen_ai.usage.cache_read.input_tokens"] == 20000
            assert attrs["gen_ai.completion"] == "async ok"

            err_resp = await http.post("http://llm.example/v1/chat/completions", json={"model": "gpt-4o"})
            assert err_resp.status_code == 500
            assert last_span(client).status.status_code == StatusCode.ERROR

    asyncio.run(_run())
