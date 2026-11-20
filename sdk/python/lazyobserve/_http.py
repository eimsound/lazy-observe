"""Isolated HTTP opener with redirects disabled.

Internal SDK telemetry uploads must never use urllib's process-global opener
(which host code may replace via ``urllib.request.install_opener``) and must
never follow HTTP 3xx redirects (which would downgrade POST to GET, silently
drop telemetry batches, or leak ``Authorization: Bearer <token>`` cross-origin).
"""

from __future__ import annotations

import urllib.request
from typing import Any, Optional


class _NoRedirectHandler(urllib.request.HTTPRedirectHandler):
    """Refuse every HTTP 3xx redirect so urllib raises HTTPError instead."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):  # noqa: ANN001, ANN201
        return None


def build_no_redirect_opener() -> urllib.request.OpenerDirector:
    """Create a standalone OpenerDirector that never follows redirects."""
    return urllib.request.build_opener(_NoRedirectHandler())


_DEFAULT_OPENER = build_no_redirect_opener()


def open_no_redirect(request: urllib.request.Request, timeout: Optional[float] = None) -> Any:
    """Execute ``request`` using the isolated no-redirect opener."""
    if timeout is None:
        return _DEFAULT_OPENER.open(request)
    return _DEFAULT_OPENER.open(request, timeout=timeout)
