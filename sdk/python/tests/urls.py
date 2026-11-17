"""List of URL patterns for tests."""

from django.urls import path


def ok_view(request):
    from django.http import JsonResponse

    return JsonResponse({"status": "ok"})


def boom_view(request):
    raise ValueError("boom")


def drf_boom_view(request):
    from rest_framework.exceptions import ValidationError

    raise ValidationError({"detail": "bad"})


def drf_server_view(request):
    from rest_framework.exceptions import APIException

    class ServerExplosion(APIException):
        status_code = 500
        default_detail = "server exploded"

    raise ServerExplosion()


urlpatterns = [
    path("ok", ok_view, name="ok"),
    path("boom", boom_view, name="boom"),
    path("api/boom", drf_boom_view, name="drf-boom"),
    path("api/server", drf_server_view, name="drf-server"),
]
