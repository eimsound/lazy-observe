"""DSN parsing, behavior-identical to the Go SDK's parseDSN."""

from __future__ import annotations

from dataclasses import dataclass
from urllib import parse


class DSNSchemeError(ValueError):
    """The DSN scheme is not http or https."""


@dataclass(frozen=True)
class DSN:
    endpoint: str
    project: str
    token: str


def parse_dsn(raw: str) -> DSN:
    if not raw:
        raise ValueError("parse DSN: empty DSN")
    url = parse.urlsplit(raw)
    if url.scheme not in ("http", "https"):
        raise ValueError("parse DSN: scheme must be http or https")
    if not url.hostname or url.username is not None or url.query or url.fragment:
        raise ValueError("parse DSN: expected http(s)://host/p/<slug>@<token>")
    path = url.path
    if not path.startswith("/p/"):
        raise ValueError("parse DSN: path must be /p/<slug>@<token>")
    rest = path[len("/p/"):]
    if "/" in rest:
        raise ValueError("parse DSN: path must be /p/<slug>@<token>")
    try:
        slug_raw, token_raw = rest.split("@", 1)
    except ValueError:
        raise ValueError("parse DSN: path must be /p/<slug>@<token>") from None
    if not slug_raw or not token_raw or "@" in token_raw:
        raise ValueError("parse DSN: path must be /p/<slug>@<token>")
    slug = parse.unquote(slug_raw)
    if not slug:
        raise ValueError("parse DSN: invalid project slug")
    token = parse.unquote(token_raw)
    if not token.strip():
        raise ValueError("parse DSN: invalid token")
    host = url.netloc
    endpoint = f"{url.scheme}://{host}"
    return DSN(endpoint=endpoint, project=slug, token=token)
