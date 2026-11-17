from django.urls import path

from .views import (
    boom,
    chat,
    db_query,
    fake_chat_completions,
    fast,
    item_list,
    ok,
    slow,
)

urlpatterns = [
    path("ok", ok, name="ok"),
    path("fast", fast, name="fast"),
    path("slow", slow, name="slow"),
    path("boom", boom, name="boom"),
    path("db", db_query, name="db"),
    path("chat", chat, name="chat"),
    path("v1/chat/completions", fake_chat_completions, name="fake-chat-completions"),
    path("api/items", item_list, name="item-list"),
]
