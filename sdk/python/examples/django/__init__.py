"""lazy-observe Python SDK example: Django + DRF.

Run:

    pip install -e ../..[test]
    python manage.py runserver 127.0.0.1:43420

The SDK is initialized from environment variables (see config/settings.py).
Routes:
    GET  /ok          fast success
    GET  /fast        fast success (logs)
    GET  /slow        ~200ms
    GET  /boom        raises -> HTTP 500 + Issue
    GET  /db          instrumented ORM query
    GET  /chat        calls the built-in fake LLM via LLM transport
    POST /api/items   DRF view (validation errors are 400, not Issues)
    POST /v1/chat/completions   fake OpenAI-compatible endpoint (no SDK)
"""
