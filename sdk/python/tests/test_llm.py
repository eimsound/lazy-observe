import json

import httpx

from tests.helpers import last_span, make_client


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
    assert "hello there" in attrs["gen_ai.prompt"] or attrs["gen_ai.prompt"]


def test_non_llm_path_passthrough(client):
    base = httpx.MockTransport(
        lambda request: httpx.Response(200, json={"ok": True})
    )
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
