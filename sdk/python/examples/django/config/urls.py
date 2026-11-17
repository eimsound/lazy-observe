"""URL configuration for the example project."""

from django.urls import include, path

urlpatterns = [
    path("", include("app.urls")),
]
