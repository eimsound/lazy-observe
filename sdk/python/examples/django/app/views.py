"""Example views exercising the lazy-observe Python SDK paths."""

import json
import logging
import time

import httpx
from django.db import connection
from django.http import JsonResponse
from rest_framework import status
from rest_framework.response import Response
from rest_framework.serializers import Serializer, CharField
from rest_framework.views import APIView

import lazy_observe

logger = logging.getLogger("lazy_observe.example")


def _body(request):
    try:
        return json.loads(request.body or b"{}")
    except ValueError:
        return {}


def ok(request):
    return JsonResponse({"status": "ok"})


def fast(request):
    logger.info("fast request handled", extra={"route": request.path})
    return JsonResponse({"status": "fast"})


def slow(request):
    time.sleep(0.2)
    return JsonResponse({"status": "slow"})


def boom(request):
    raise ValueError("example boom: not a real outage")


def db_query(request):
    client = lazy_observe.get_client()
    with client.django_db():
        with connection.cursor() as cursor:
            cursor.execute("SELECT 1 AS one, 'lazy-observe' AS who")
            row = cursor.fetchone()
    return JsonResponse({"row": list(row)})


def chat(request):
    """Explicitly wrap an httpx client with the LLM transport and call the
    built-in fake OpenAI-compatible endpoint on this same server."""
    client = lazy_observe.get_client()
    base_url = "http://127.0.0.1:" + request.META.get("HTTP_HOST", "127.0.0.1").split(":")[-1]

    with httpx.Client(transport=client.llm_transport()) as http:
        payload = {"model": "example-model", "messages": [{"role": "user", "content": "hi"}]}
        if request.method == "POST":
            payload = _body(request) or payload
        response = http.post(
            f"{base_url}/v1/chat/completions",
            json=payload,
            timeout=10.0,
        )
    return JsonResponse(response.json())


def fake_chat_completions(request):
    """OpenAI-compatible response, so the LLM transport records usage."""
    body = _body(request)
    model = body.get("model", "example-model")
    return JsonResponse(
        {
            "id": "chatcmpl-example",
            "object": "chat.completion",
            "model": model,
            "choices": [
                {
                    "index": 0,
                    "message": {"role": "assistant", "content": "hello from fake llm"},
                    "finish_reason": "stop",
                }
            ],
            "usage": {"prompt_tokens": 12, "completion_tokens": 6, "total_tokens": 18},
        }
    )


class ItemSerializer(Serializer):
    name = CharField()


class ItemListView(APIView):
    def get(self, request):
        return Response({"items": []})

    def post(self, request):
        serializer = ItemSerializer(data=request.data)
        if not serializer.is_valid():
            return Response(serializer.errors, status=status.HTTP_400_BAD_REQUEST)
        return Response({"created": serializer.validated_data}, status=status.HTTP_201_CREATED)


item_list = ItemListView.as_view()
