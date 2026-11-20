"""Explicit LLM instrumentation for OpenAI-compatible, Anthropic, and Ollama
HTTP calls, mirroring Go's ``lo.LLMTransport``. Wraps an httpx transport
(no patching of provider SDKs or the default client).

Usage:

    import httpx
    client = httpx.Client(transport=lo.llm_transport())
    # pass as http_client=... to openai / anthropic SDKs
"""

from __future__ import annotations

import codecs
import json
import math
import threading
import typing as t
from urllib.parse import urlsplit

import httpx
from opentelemetry import context as otel_context
from opentelemetry import trace as otel_trace

LLM_CONTENT_LIMIT = 4 * 1024
LLM_BODY_INSPECTION_LIMIT = 1 << 20
_MAX_RETAINED_COMPLETION_BYTES = LLM_CONTENT_LIMIT + 4

_OPENAI_ANY = ("/chat/completions",)
_ANTHROPIC_ANY = ("/v1/messages",)
_OLLAMA_ANY = ("/api/chat",)


def detect_llm_system(path: str) -> t.Optional[str]:
    normalized = path.rstrip("/")
    for suffix in _OPENAI_ANY:
        if normalized.endswith(suffix):
            return "openai"
    for suffix in _ANTHROPIC_ANY:
        if normalized.endswith(suffix):
            return "anthropic"
    for suffix in _OLLAMA_ANY:
        if normalized.endswith(suffix):
            return "ollama"
    return None


def _truncate(value: str) -> str:
    encoded = value.encode("utf-8")
    if len(encoded) <= LLM_CONTENT_LIMIT:
        return value
    return encoded[:LLM_CONTENT_LIMIT].decode("utf-8", errors="ignore")


def _prompt_text(payload: dict) -> str:
    if "prompt" in payload:
        if isinstance(payload["prompt"], str):
            return _truncate(payload["prompt"])
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


def _openai_stream_delta(payload: dict) -> str:
    choices = payload.get("choices")
    if not isinstance(choices, list):
        return ""
    parts = []
    for choice in choices:
        if isinstance(choice, dict):
            delta = choice.get("delta")
            if isinstance(delta, dict) and delta.get("content") is not None:
                parts.append(str(delta["content"]))
    return "".join(parts)


def _int_token(value: t.Any) -> t.Optional[int]:
    if isinstance(value, bool):
        return None
    if isinstance(value, int):
        return value
    if isinstance(value, float) and math.isfinite(value):
        return int(value)
    return None


def _first_int_token(*values: t.Any) -> t.Optional[int]:
    for value in values:
        parsed = _int_token(value)
        if parsed is not None:
            return parsed
    return None


def _usage_attrs(system: str, payload: dict) -> t.Dict[str, t.Union[int, bool]]:
    attrs: t.Dict[str, t.Union[int, bool]] = {}
    input_tokens: t.Optional[int] = None
    output_tokens: t.Optional[int] = None
    cache_read_tokens: t.Optional[int] = None
    cache_creation_tokens: t.Optional[int] = None
    reasoning_tokens: t.Optional[int] = None

    usage = payload.get("usage")
    if not isinstance(usage, dict):
        if system == "ollama":
            input_tokens = _int_token(payload.get("prompt_eval_count"))
            output_tokens = _int_token(payload.get("eval_count"))
    else:
        input_tokens = _first_int_token(usage.get("prompt_tokens"), usage.get("input_tokens"))
        output_tokens = _first_int_token(usage.get("completion_tokens"), usage.get("output_tokens"))
        if system == "anthropic":
            cache_read_tokens = _int_token(usage.get("cache_read_input_tokens"))
            cache_creation_tokens = _int_token(usage.get("cache_creation_input_tokens"))
        else:
            prompt_details = usage.get("prompt_tokens_details")
            prompt_cached = prompt_details.get("cached_tokens") if isinstance(prompt_details, dict) else None
            cache_read_tokens = _first_int_token(prompt_cached, usage.get("cache_read_input_tokens"))
            completion_details = usage.get("completion_tokens_details")
            if isinstance(completion_details, dict):
                reasoning_tokens = _int_token(completion_details.get("reasoning_tokens"))

    if input_tokens is not None:
        attrs["gen_ai.usage.input_tokens"] = input_tokens
    if output_tokens is not None:
        attrs["gen_ai.usage.output_tokens"] = output_tokens
    if cache_read_tokens is not None:
        attrs["gen_ai.usage.cache_read.input_tokens"] = cache_read_tokens
    if cache_creation_tokens is not None:
        attrs["gen_ai.usage.cache_creation.input_tokens"] = cache_creation_tokens
    if reasoning_tokens is not None:
        attrs["gen_ai.usage.reasoning_tokens"] = reasoning_tokens
    if input_tokens is None or output_tokens is None:
        attrs["gen_ai.usage.missing"] = True
    return attrs


def _response_model(system: str, payload: dict) -> t.Optional[str]:
    del system
    value = payload.get("model")
    if isinstance(value, str) and value:
        return value
    return None


def _read_request_payload(request: httpx.Request) -> dict:
    try:
        raw = request.content
    except httpx.RequestNotRead:
        return {}
    if not raw or len(raw) > LLM_BODY_INSPECTION_LIMIT:
        return {}
    try:
        parsed = json.loads(raw)
        return parsed if isinstance(parsed, dict) else {}
    except (ValueError, UnicodeDecodeError):
        return {}


def _is_streaming_response(response: httpx.Response, requested: bool) -> bool:
    if response.status_code < 200 or response.status_code >= 300:
        return False
    content_type = response.headers.get("content-type", "").lower()
    return requested or "text/event-stream" in content_type or "application/x-ndjson" in content_type


class _LLMStreamAccumulator:
    """Bounded SSE/NDJSON parser shared by sync and async httpx streams."""

    def __init__(self, system: str, span: t.Any, capture_content: bool) -> None:
        self._system = system
        self._span = span
        self._capture_content = capture_content
        self._decoder = codecs.getincrementaldecoder("utf-8")(errors="replace")
        self._pending = ""
        self._discarding_line = False
        self._lock = threading.Lock()
        self._finished = False

        self._model: t.Optional[str] = None
        self._input_tokens: t.Optional[int] = None
        self._output_tokens: t.Optional[int] = None
        self._cache_read_tokens: t.Optional[int] = None
        self._cache_creation_tokens: t.Optional[int] = None
        self._reasoning_tokens: t.Optional[int] = None
        self._completion_parts: t.List[str] = []
        self._completion_bytes = 0

    def feed(self, chunk: bytes) -> None:
        if not chunk:
            return
        with self._lock:
            if self._finished:
                return
            self._consume_text(self._decoder.decode(chunk, final=False))

    def _consume_text(self, text: str) -> None:
        offset = 0
        length = len(text)
        while offset < length:
            newline = text.find("\n", offset)
            if self._discarding_line:
                if newline < 0:
                    return
                offset = newline + 1
                self._discarding_line = False
                continue
            if newline < 0:
                tail = text[offset:]
                if len(self._pending) + len(tail) > LLM_BODY_INSPECTION_LIMIT:
                    self._pending = ""
                    self._discarding_line = True
                    return
                self._pending += tail
                return
            part = text[offset:newline]
            offset = newline + 1
            if len(self._pending) + len(part) > LLM_BODY_INSPECTION_LIMIT:
                self._pending = ""
                continue
            line = self._pending + part if self._pending else part
            self._pending = ""
            self._parse_line(line)

    def _append_completion(self, chunk: str) -> None:
        if not self._capture_content or not chunk or self._completion_bytes >= _MAX_RETAINED_COMPLETION_BYTES:
            return
        encoded = chunk.encode("utf-8")
        if self._completion_bytes + len(encoded) <= _MAX_RETAINED_COMPLETION_BYTES:
            self._completion_parts.append(chunk)
            self._completion_bytes += len(encoded)
            return
        remaining = _MAX_RETAINED_COMPLETION_BYTES - self._completion_bytes
        self._completion_parts.append(encoded[:remaining].decode("utf-8", errors="ignore"))
        self._completion_bytes = _MAX_RETAINED_COMPLETION_BYTES

    def _parse_line(self, raw_line: str) -> None:
        line = raw_line.strip()
        if line.startswith("data:"):
            line = line[5:].strip()
        if not line or line == "[DONE]" or not line.startswith("{"):
            return
        try:
            data = json.loads(line)
        except ValueError:
            return
        if not isinstance(data, dict):
            return

        if model := _response_model(self._system, data):
            self._model = model

        usage_attrs = _usage_attrs(self._system, data)
        if "gen_ai.usage.input_tokens" in usage_attrs:
            self._input_tokens = int(usage_attrs["gen_ai.usage.input_tokens"])
        if "gen_ai.usage.output_tokens" in usage_attrs:
            self._output_tokens = int(usage_attrs["gen_ai.usage.output_tokens"])
        if "gen_ai.usage.cache_read.input_tokens" in usage_attrs:
            self._cache_read_tokens = int(usage_attrs["gen_ai.usage.cache_read.input_tokens"])
        if "gen_ai.usage.cache_creation.input_tokens" in usage_attrs:
            self._cache_creation_tokens = int(usage_attrs["gen_ai.usage.cache_creation.input_tokens"])
        if "gen_ai.usage.reasoning_tokens" in usage_attrs:
            self._reasoning_tokens = int(usage_attrs["gen_ai.usage.reasoning_tokens"])

        if self._capture_content:
            self._append_completion(_completion_text(self._system, data))
            if self._system == "openai":
                self._append_completion(_openai_stream_delta(data))

        if self._system == "anthropic":
            message = data.get("message")
            if isinstance(message, dict):
                if not self._model and isinstance(message.get("model"), str) and message["model"]:
                    self._model = message["model"]
                msg_usage = message.get("usage")
                if isinstance(msg_usage, dict):
                    if (val := _int_token(msg_usage.get("input_tokens"))) is not None:
                        self._input_tokens = val
                    if (val := _int_token(msg_usage.get("cache_read_input_tokens"))) is not None:
                        self._cache_read_tokens = val
                    if (val := _int_token(msg_usage.get("cache_creation_input_tokens"))) is not None:
                        self._cache_creation_tokens = val
            delta = data.get("delta")
            if self._capture_content and isinstance(delta, dict) and isinstance(delta.get("text"), str):
                self._append_completion(delta["text"])

    def finish(self, error: t.Optional[BaseException] = None) -> None:
        with self._lock:
            if self._finished:
                return
            self._finished = True
            tail = self._decoder.decode(b"", final=True)
            if tail:
                self._consume_text(tail)
            if not self._discarding_line and self._pending.strip():
                self._parse_line(self._pending)
            self._pending = ""
            model = self._model
            input_tokens = self._input_tokens
            output_tokens = self._output_tokens
            cache_read = self._cache_read_tokens
            cache_creation = self._cache_creation_tokens
            reasoning = self._reasoning_tokens
            completion = _truncate("".join(self._completion_parts)) if self._capture_content else ""

        if error is not None:
            if isinstance(error, Exception):
                self._span.record_exception(error)
            self._span.set_status(otel_trace.StatusCode.ERROR, str(error))
        if model:
            self._span.set_attribute("gen_ai.response.model", model)
        if input_tokens is not None:
            self._span.set_attribute("gen_ai.usage.input_tokens", input_tokens)
        if output_tokens is not None:
            self._span.set_attribute("gen_ai.usage.output_tokens", output_tokens)
        if cache_read is not None:
            self._span.set_attribute("gen_ai.usage.cache_read.input_tokens", cache_read)
        if cache_creation is not None:
            self._span.set_attribute("gen_ai.usage.cache_creation.input_tokens", cache_creation)
        if reasoning is not None:
            self._span.set_attribute("gen_ai.usage.reasoning_tokens", reasoning)
        if input_tokens is None or output_tokens is None:
            self._span.set_attribute("gen_ai.usage.missing", True)
        if completion:
            self._span.set_attribute("gen_ai.completion", completion)
        self._span.end()


class _LLMByteStream(httpx.SyncByteStream):
    def __init__(self, stream: httpx.SyncByteStream, accumulator: _LLMStreamAccumulator) -> None:
        self._stream = stream
        self._acc = accumulator

    def __iter__(self) -> t.Iterator[bytes]:
        try:
            for chunk in self._stream:
                self._acc.feed(chunk)
                yield chunk
        except BaseException as exc:
            self._acc.finish(exc)
            raise
        else:
            self._acc.finish()

    def close(self) -> None:
        try:
            self._stream.close()
        finally:
            self._acc.finish()


class _AsyncLLMByteStream(httpx.AsyncByteStream):
    def __init__(self, stream: httpx.AsyncByteStream, accumulator: _LLMStreamAccumulator) -> None:
        self._stream = stream
        self._acc = accumulator

    async def __aiter__(self) -> t.AsyncIterator[bytes]:
        try:
            async for chunk in self._stream:
                self._acc.feed(chunk)
                yield chunk
        except BaseException as exc:
            self._acc.finish(exc)
            raise
        else:
            self._acc.finish()

    async def aclose(self) -> None:
        try:
            await self._stream.aclose()
        finally:
            self._acc.finish()


def _build_span_attributes(system: str, payload: dict, request_model: str, capture_content: bool) -> t.Dict[str, t.Any]:
    attrs: t.Dict[str, t.Any] = {
        "gen_ai.operation.name": "chat",
        "gen_ai.provider.name": system,
        "gen_ai.system": system,
    }
    if request_model:
        attrs["gen_ai.request.model"] = request_model
    if capture_content:
        prompt = _prompt_text(payload)
        if prompt:
            attrs["gen_ai.prompt"] = prompt
    return attrs


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
        return _build_span_attributes(system, payload, request_model, self._capture_content)

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        system = detect_llm_system(urlsplit(str(request.url)).path)
        if not system:
            return self._base.handle_request(request)

        payload = _read_request_payload(request)
        streaming = bool(payload.get("stream"))
        request_model = payload.get("model") if isinstance(payload.get("model"), str) else ""

        attrs = self._span_attributes(system, payload, request_model)
        span = self._tracer.start_span(
            f"chat {system}",
            kind=otel_trace.SpanKind.CLIENT,
            attributes=attrs,
        )
        token = otel_context.attach(otel_trace.set_span_in_context(span))
        span_owned_by_stream = False
        try:
            response = self._base.handle_request(request)
            if response.status_code >= 400:
                span.set_status(otel_trace.StatusCode.ERROR, f"HTTP {response.status_code}")
            if _is_streaming_response(response, streaming):
                acc = _LLMStreamAccumulator(system, span, self._capture_content)
                wrapped = httpx.Response(
                    status_code=response.status_code,
                    headers=response.headers,
                    stream=_LLMByteStream(response.stream, acc),
                    extensions=response.extensions if self.preserve_extensions else None,
                )
                span_owned_by_stream = True
                return wrapped
            try:
                response.read()
                body = response.content
                if body and len(body) <= LLM_BODY_INSPECTION_LIMIT:
                    apply_llm_response_attributes(span, system, body, self._capture_content, False)
                else:
                    span.set_attribute("gen_ai.usage.missing", True)
            except Exception:  # noqa: BLE001 - parsing must not break the call
                span.set_attribute("gen_ai.usage.missing", True)
            return response
        except BaseException as exc:
            if isinstance(exc, Exception):
                span.record_exception(exc)
            span.set_status(otel_trace.StatusCode.ERROR, str(exc))
            raise
        finally:
            otel_context.detach(token)
            if not span_owned_by_stream:
                span.end()

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

        payload = _read_request_payload(request)
        streaming = bool(payload.get("stream"))
        request_model = payload.get("model") if isinstance(payload.get("model"), str) else ""
        attrs = _build_span_attributes(system, payload, request_model, self._capture_content)
        span = self._tracer.start_span(
            f"chat {system}",
            kind=otel_trace.SpanKind.CLIENT,
            attributes=attrs,
        )
        token = otel_context.attach(otel_trace.set_span_in_context(span))
        span_owned_by_stream = False
        try:
            response = await self._base.handle_async_request(request)
            if response.status_code >= 400:
                span.set_status(otel_trace.StatusCode.ERROR, f"HTTP {response.status_code}")
            if _is_streaming_response(response, streaming):
                acc = _LLMStreamAccumulator(system, span, self._capture_content)
                wrapped = httpx.Response(
                    status_code=response.status_code,
                    headers=response.headers,
                    stream=_AsyncLLMByteStream(response.stream, acc),
                    extensions=response.extensions,
                )
                span_owned_by_stream = True
                return wrapped
            try:
                await response.aread()
                body = response.content
                if body and len(body) <= LLM_BODY_INSPECTION_LIMIT:
                    apply_llm_response_attributes(span, system, body, self._capture_content, False)
                else:
                    span.set_attribute("gen_ai.usage.missing", True)
            except Exception:  # noqa: BLE001
                span.set_attribute("gen_ai.usage.missing", True)
            return response
        except BaseException as exc:
            if isinstance(exc, Exception):
                span.record_exception(exc)
            span.set_status(otel_trace.StatusCode.ERROR, str(exc))
            raise
        finally:
            otel_context.detach(token)
            if not span_owned_by_stream:
                span.end()

    async def aclose(self) -> None:
        await self._base.aclose()


def apply_llm_response_attributes(span, system: str, body: bytes, capture_content: bool, streaming: bool) -> None:
    del streaming
    try:
        data = json.loads(body)
    except (ValueError, UnicodeDecodeError):
        span.set_attribute("gen_ai.usage.missing", True)
        return
    if not isinstance(data, dict):
        span.set_attribute("gen_ai.usage.missing", True)
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
