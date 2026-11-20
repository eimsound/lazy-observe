#!/usr/bin/env bash
set -euo pipefail

# Verify runtime (and optional django extra) dependencies installed for
# sdk/python (lazyobserve) satisfy the permissive license allowlist.
# Uses only the Python standard library inside the uv-managed venv.

python_bin=${1:-sdk/python/.venv/bin/python}
if [[ ! -x $python_bin ]]; then
  printf 'FAIL: sdk/python: missing Python interpreter %s (run `mise run sdk-python` first)\n' "$python_bin" >&2
  exit 1
fi

"$python_bin" - <<'PY'
import importlib.metadata as md
import re
import sys

ALLOWED_SPDX = {
    "Apache-2.0",
    "MIT",
    "BSD-2-Clause",
    "BSD-3-Clause",
    "0BSD",
    "ISC",
    "Unlicense",
    "PSF-2.0",
}

# Package-scoped weak-copyleft exception: certifi ships Mozilla's CA root bundle
# under MPL-2.0 and is pulled transitively by opentelemetry-exporter-otlp-proto-http -> requests.
# Any other MPL-2.0 package or any GPL/AGPL/LGPL/unknown license must fail.
PACKAGE_EXCEPTIONS = {
    "certifi": {"MPL-2.0"},
}

LEGACY_TO_SPDX = {
    "apache 2.0": "Apache-2.0",
    "apache-2.0": "Apache-2.0",
    "apache software license": "Apache-2.0",
    "apache license 2.0": "Apache-2.0",
    "mit": "MIT",
    "mit license": "MIT",
    "bsd-2-clause": "BSD-2-Clause",
    "bsd-3-clause": "BSD-3-Clause",
    "3-clause bsd license": "BSD-3-Clause",
    "2-clause bsd license": "BSD-2-Clause",
    "bsd license": "BSD-3-Clause",
    "isc": "ISC",
    "isc license": "ISC",
    "psf-2.0": "PSF-2.0",
    "python software foundation license": "PSF-2.0",
    "mpl-2.0": "MPL-2.0",
    "mozilla public license 2.0 (mpl 2.0)": "MPL-2.0",
}

CLASSIFIER_TO_SPDX = {
    "License :: OSI Approved :: Apache Software License": "Apache-2.0",
    "License :: OSI Approved :: MIT License": "MIT",
    "License :: OSI Approved :: BSD License": "BSD-3-Clause",
    "License :: OSI Approved :: ISC License (ISCL)": "ISC",
    "License :: OSI Approved :: Python Software Foundation License": "PSF-2.0",
    "License :: OSI Approved :: Mozilla Public License 2.0 (MPL 2.0)": "MPL-2.0",
}


def norm(name: str) -> str:
    return re.sub(r"[-_.]+", "-", name).lower()


def extract_licenses(dist: md.Distribution) -> list[str]:
    expr = (dist.metadata.get("License-Expression") or "").strip()
    if expr:
        tokens = [
            token.strip().strip("()").strip()
            for token in re.split(r"\s+(?:OR|AND)\s+", expr)
            if token.strip().strip("()").strip()
        ]
        if tokens:
            return tokens
    raw = (dist.metadata.get("License") or "").strip()
    if raw:
        first_line = raw.splitlines()[0].strip()
        mapped = LEGACY_TO_SPDX.get(first_line.lower())
        if mapped:
            return [mapped]
        if first_line in ALLOWED_SPDX:
            return [first_line]
    classifiers = [
        c.strip()
        for c in (dist.metadata.get_all("Classifier") or [])
        if c.strip().startswith("License ::")
    ]
    mapped_classifiers = [CLASSIFIER_TO_SPDX[c] for c in classifiers if c in CLASSIFIER_TO_SPDX]
    if mapped_classifiers:
        return mapped_classifiers
    if raw:
        return [raw.splitlines()[0].strip()]
    if classifiers:
        return classifiers
    return ["<unknown license>"]


def should_follow_req(req: str, root_package: bool) -> bool:
    if ";" not in req:
        return True
    marker = req.split(";", 1)[1]
    extra_matches = re.findall(r"""extra\s*==\s*["']([^"']+)["']""", marker)
    if not extra_matches:
        return True
    # Follow optional django extra on the root package; skip test/dev extras everywhere.
    return root_package and all(e == "django" for e in extra_matches)


try:
    md.distribution("lazyobserve")
except md.PackageNotFoundError:
    print("FAIL: sdk/python: lazyobserve is not installed in the target environment", file=sys.stderr)
    raise SystemExit(1)

seen: set[str] = set()
queue: list[tuple[str, bool]] = [("lazyobserve", True)]
violations: list[str] = []
checked = 0

while queue:
    raw_name, is_root = queue.pop(0)
    pkg = norm(raw_name)
    if pkg in seen:
        continue
    seen.add(pkg)
    try:
        dist = md.distribution(pkg)
    except md.PackageNotFoundError:
        # Environment-marked dependencies for other Python versions/platforms are not installed.
        continue

    checked += 1
    pkg_name = dist.metadata["Name"]
    licenses = extract_licenses(dist)
    expr = (dist.metadata.get("License-Expression") or "").strip()
    allowed_for_pkg = ALLOWED_SPDX | PACKAGE_EXCEPTIONS.get(pkg, set())

    # If the expression is a disjunction ("A OR B"), any allowed branch suffices;
    # otherwise every declared license must be in the allowlist.
    is_disjunction = " OR " in expr and " AND " not in expr
    ok = (
        any(lic in allowed_for_pkg for lic in licenses)
        if is_disjunction
        else all(lic in allowed_for_pkg for lic in licenses)
    )
    if not ok:
        violations.append(f"{pkg_name}=={dist.version}: {', '.join(licenses)}")

    for req in dist.metadata.get_all("Requires-Dist") or []:
        if not should_follow_req(req, is_root):
            continue
        dep = re.split(r"[ ;(<>=!~\[]", req, maxsplit=1)[0].strip()
        if dep:
            queue.append((dep, False))

if checked <= 1:
    print("FAIL: sdk/python: expected transitive runtime dependencies to be inspected", file=sys.stderr)
    raise SystemExit(1)

if violations:
    print(
        "FAIL: sdk/python: production dependency license violations:\n  "
        + "\n  ".join(sorted(violations)),
        file=sys.stderr,
    )
    raise SystemExit(1)

print(f"PASS: sdk/python ({checked} packages) production dependencies satisfy the Python license allowlist")
PY
