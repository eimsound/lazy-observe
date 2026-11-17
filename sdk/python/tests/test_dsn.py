import pytest

from lazyobserve.dsn import parse_dsn


def test_valid_dsn():
    dsn = parse_dsn("http://127.0.0.1:3100/p/myapp@token123")
    assert dsn.endpoint == "http://127.0.0.1:3100"
    assert dsn.project == "myapp"
    assert dsn.token == "token123"


def test_valid_dsn_https():
    dsn = parse_dsn("https://example.com:8443/p/slug@secr3t")
    assert dsn.endpoint == "https://example.com:8443"
    assert dsn.project == "slug"
    assert dsn.token == "secr3t"


def test_escaped_slug_and_token():
    dsn = parse_dsn("http://h/p/my%20app@tok%40en")
    assert dsn.project == "my app"
    assert dsn.token == "tok@en"


@pytest.mark.parametrize(
    "bad",
    [
        "",
        "ftp://host/p/a@b",
        "http:///p/a@b",
        "http://host/p/a@b?q=1",
        "http://host/p/a@b#frag",
        "http://user:pw@host/p/a@b",
        "http://host/x/a@b",
        "http://host/p/a@b/c",
        "http://host/p/@b",
        "http://host/p/a@",
        "http://host/p/a@b@c",
    ],
)
def test_invalid_dsn(bad):
    with pytest.raises(ValueError):
        parse_dsn(bad)
