"""Explicit LLM instrumentation for OpenAI-compatible, Anthropic, and Ollama
HTTP calls, mirroring Go's ``lo.LLMTransport``. Wraps an httpx transport
(no patching of provider SDKs or the default client).

Usage:

    import httpx
    client = httpx.Client(transport=lo.llm_transport())
    # pass as http_client=... to openai / anthropic SDKs
"""

from __future__ import annotations

import json
import typing as t
from urllib.parse import urlsplit

import httpx
from opentelemetry import context as otel_context
from opentelemetry import trace as otel_trace

LLM_CONTENT_LIMIT = 4 * 1024

_OPENAI_ANY = ("/chat/completions",)
_ANTHROPIC_ANY = ("/v1/messages",)
_OLLAMA_ANY = ("/api/chat",)


def detect_llm_system(path: str) -> t.Optional[str]:
    for suffix in _OPENAI_ANY:
        if path.endswith(suffix):
            return "openai"
    for suffix in _ANTHROPIC_ANY:
        if path.endswith(suffix):
            return "anthropic"
    for suffix in _OLLAMA_ANY:
        if path.endswith(suffix):
            return "ollama"
    return None


def _truncate(value: str) -> str:
    return value[:LLM_CONTENT_LIMIT]


def _prompt_text(payload: dict) -> str:
    if "prompt" in payload:
        if isinstance(payload["prompt"], str):
            return payload["prompt"]
        if isinstance(payload["prompt"], list):
            return _truncate(json.dumps(payload["prompt"], ensure_ascii=False))
    messages = payload.get("messages")
    if isinstance(messages, list):
        return _truncate(json.dumps(messages, ensure_ascii=False))
    system = payload.get("system")
    if isinstance(system, str):
        return _truncate(system)
    return ""


def _completion_text(system: str, payload: dict) -> str:
    if system == "anthropic":
        content = payload.get("content") or []
        parts = []
        for block in content:
            if isinstance(block, dict) and block.get("type") in ("text",):
                parts.append(str(block.get("text", "")))
        return _truncate("".join(parts))
    if system == "ollama":
        if isinstance(payload.get("message"), dict):
            return _truncate(str(payload["message"].get("content", "")))
    if isinstance(payload.get("choices"), list):
        parts = []
        for choice in payload["choices"]:
            if isinstance(choice, dict):
                message = choice.get("message")
                if isinstance(message, dict) and message.get("content") is not None:
                    parts.append(str(message["content"]))
        return _truncate("".join(parts))
    if isinstance(payload.get("response"), str):
        return _truncate(payload["response"])
    return ""


def _usage_attrs(system: str, payload: dict) -> t.Dict[str, t.Union[int, bool]]:
    usage = payload.get("usage")
    attrs: t.Dict[str, t.Union[int, bool]] = {}
    if not isinstance(usage, dict):
        if system == "ollama":
            prompt_eval = payload.get("prompt_eval_count")
            eval_count = payload.get("eval_count")
            attrs = {}
            if isinstance(prompt_eval, (int, float)):
                attrs["gen_ai.usage.input_tokens"] = int(prompt_eval)
            if isinstance(eval_count, (int, float)):
                attrs["gen_ai.usage.output_tokens"] = int(eval_count)
        if not attrs:
            attrs["gen_ai.usage.missing"] = True
        return attrs
    input_tokens = usage.get("prompt_tokens") or usage.get("input_tokens")
    output_tokens = usage.get("completion_tokens") or usage.get("output_tokens")
    if isinstance(input_tokens, (int, float)):
        attrs["gen_ai.usage.input_tokens"] = int(input_tokens)
    if isinstance(output_tokens, (int, float)):
        attrs["gen_ai.usage.output_tokens"] = int(output_tokens)
    if not attrs:
        attrs["gen_ai.usage.missing"] = True
    return attrs


def _response_model(system: str, payload: dict) -> t.Optional[str]:
    for key in ("model",):
        value = payload.get(key)
        if isinstance(value, str) and value:
            return value
    return None


class LLMTransport(httpx.BaseTransport):
    """Synchronous httpx transport with lazy-observe LLM spans."""

    def __init__(
        self,
        client: t.Any,
        base: t.Optional[httpx.BaseTransport] = None,
        preserve_extensions: bool = True,
    ) -> None:
        self._client = client
        self._base = base or httpx.HTTPTransport()
        self._tracer = client.tracer_provider.get_tracer("io.lazyobserve.python")
        self._capture_content = bool(getattr(client.config, "capture_llm_content", False))
        self.preserve_extensions = preserve_extensions

    def _span_attributes(self, system: str, payload: dict, request_model: str) -> t.Dict[str, t.Any]:
        attrs: t.Dict[str, t.Any] = {
            "gen_ai.operation.name": "chat",
            "gen_ai.provider.name": system,
            "gen_ai.system": system,
        }
        if request_model:
            attrs["gen_ai.request.model"] = request_model
        if self._capture_content:
            prompt = _prompt_text(payload)
            if prompt:
                attrs["gen_ai.prompt"] = prompt
        return attrs

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        system = detect_llm_system(urlsplit(str(request.url)).path)
        if not system:
            return self._base.handle_request(request)

        payload: dict = {}
        if request.content:
            try:
                payload = json.loads(request.content)
            except (ValueError, UnicodeDecodeError):
                payload = {}
        streaming = bool(payload.get("stream"))
        request_model = payload.get("model") if isinstance(payload.get("model"), str) else ""

        attrs = self._span_attributes(system, payload, request_model)
        span = self._tracer.start_span(
            f"chat {system}",
            kind=otel_trace.SpanKind.CLIENT,
            attributes=attrs,
        )
        token = otel_context.attach(otel_trace.set_span_in_context(span))
        try:
            response = self._base.handle_request(request)
            if streaming:
                span.set_attribute("gen_ai.usage.missing", True)
            else:
                try:
                    # 真实 HTTPTransport 返回的响应是未读的流，直接访问
                    # .content 会抛 ResponseNotRead，导致 usage 解析失败；
                    # 先 read() 再解析。
                    response.read()
                    body = response.content
                    if body:
                        apply_llm_response_attributes(
                            span, system, body, self._capture_content, streaming
                        )
                    else:
                        span.set_attribute("gen_ai.usage.missing", True)
                except Exception:  # noqa: BLE001 - parsing must not break the call
                    span.set_attribute("gen_ai.usage.missing", True)
            return response
        finally:
            span.end()
            otel_context.detach(token)

    def close(self) -> None:
        self._base.close()


class AsyncLLMTransport(httpx.AsyncBaseTransport):
    """Asynchronous httpx transport with lazy-observe LLM spans."""

    def __init__(
        self,
        client: t.Any,
        base: t.Optional[httpx.AsyncBaseTransport] = None,
    ) -> None:
        self._client = client
        self._base = base or httpx.AsyncHTTPTransport()
        self._tracer = client.tracer_provider.get_tracer("io.lazyobserve.python")
        self._capture_content = bool(getattr(client.config, "capture_llm_content", False))

    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        system = detect_llm_system(urlsplit(str(request.url)).path)
        if not system:
            return await self._base.handle_async_request(request)
        payload: dict = {}
        if request.content:
            try:
                payload = json.loads(request.content)
            except (ValueError, UnicodeDecodeError):
                payload = {}
        streaming = bool(payload.get("stream"))
        request_model = payload.get("model") if isinstance(payload.get("model"), str) else ""
        attrs: t.Dict[str, t.Any] = {
            "gen_ai.operation.name": "chat",
            "gen_ai.provider.name": system,
            "gen_ai.system": system,
        }
        if request_model:
            attrs["gen_ai.request.model"] = request_model
        if self._capture_content:
            prompt = _prompt_text(payload)
            if prompt:
                attrs["gen_ai.prompt"] = prompt
        span = self._tracer.start_span(
            f"chat {system}",
            kind=otel_trace.SpanKind.CLIENT,
            attributes=attrs,
        )
        token = otel_context.attach(otel_trace.set_span_in_context(span))
        try:
            response = await self._base.handle_async_request(request)
            if streaming:
                span.set_attribute("gen_ai.usage.missing", True)
            else:
                try:
                    # 同同步版本：先读响应流再解析 usage。
                    await response.aread()
                    body = response.content
                    if body:
                        apply_llm_response_attributes(span, system, body, self._capture_content, streaming)
                    else:
                        span.set_attribute("gen_ai.usage.missing", True)
                except Exception:  # noqa: BLE001
                    span.set_attribute("gen_ai.usage.missing", True)
            return response
        finally:
            span.end()
            otel_context.detach(token)

    async def aclose(self) -> None:
        await self._base.aclose()


def apply_llm_response_attributes(span, system: str, body: bytes, capture_content: bool, streaming: bool) -> None:
    try:
        data = json.loads(body)
    except (ValueError, UnicodeDecodeError):
        return
    if response_model := _response_model(system, data):
        span.set_attribute("gen_ai.response.model", response_model)
    for key, value in _usage_attrs(system, data).items():
        span.set_attribute(key, value)
    if capture_content:
        completion = _completion_text(system, data)
        if completion:
            span.set_attribute("gen_ai.completion", completion)


def build_llm_transport(client: t.Any, base: t.Any = None, client_kind: str = "sync"):
    if client_kind == "async":
        return AsyncLLMTransport(client, base=base)
    return LLMTransport(client, base=base)
