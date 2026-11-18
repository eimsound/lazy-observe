"""Write the committed pprof fixture consumed by the Go read-back test."""

from __future__ import annotations

from tests.pprof_fixture import REPO_ROOT, write_fixture


def main() -> None:
    path = write_fixture()
    print(f"wrote {path.relative_to(REPO_ROOT)}")


if __name__ == "__main__":
    main()
