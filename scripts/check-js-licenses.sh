#!/usr/bin/env bash
set -euo pipefail

if (($# == 0)); then
  printf 'usage: %s <workspace> [workspace ...]\n' "$0" >&2
  exit 2
fi

pnpm_command=${PNPM:-pnpm}
command -v "$pnpm_command" >/dev/null 2>&1 || {
  printf 'missing pnpm command: %s\n' "$pnpm_command" >&2
  exit 1
}
command -v node >/dev/null 2>&1 || {
  printf 'missing node command required to validate pnpm license output\n' >&2
  exit 1
}

work=$(mktemp -d "${TMPDIR:-/tmp}/lazy-observe-js-licenses.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM

index=0
for workspace in "$@"; do
  if [[ ! -d $workspace || ! -f $workspace/package.json ]]; then
    printf 'invalid JavaScript workspace: %s\n' "$workspace" >&2
    exit 1
  fi

  index=$((index + 1))
  output="$work/licenses-$index.json"
  # pnpm 11 起 licenses list 在工作区根只列根包，--recursive 才覆盖 packages/*、examples/*（单包工程结果不变）。
  if ! (cd "$workspace" && "$pnpm_command" licenses list --prod --json --recursive) >"$output"; then
    printf 'failed to list production dependency licenses for %s\n' "$workspace" >&2
    exit 1
  fi

  WORKSPACE=$workspace LICENSE_JSON=$output node <<'NODE'
const fs = require("node:fs");

const workspace = process.env.WORKSPACE;
const path = process.env.LICENSE_JSON;
const allowed = new Set([
  "Apache-2.0",
  "MIT",
  "BSD-2-Clause",
  "BSD-3-Clause",
  "0BSD",
  "ISC",
  // Permissive public-domain dedication; ships alongside MIT (e.g. isbot via @tanstack/react-router).
  "Unlicense",
]);

const fail = (message) => {
  console.error(`FAIL: ${workspace}: ${message}`);
  process.exit(1);
};

const raw = fs.readFileSync(path, "utf8");
if (raw.trim() === "") {
  fail("pnpm produced empty license output");
}

let groups;
try {
  groups = JSON.parse(raw);
} catch (error) {
  fail(`pnpm produced invalid license JSON: ${error.message}`);
}
if (groups === null || Array.isArray(groups) || typeof groups !== "object") {
  fail("pnpm license JSON must be an object grouped by license");
}

const licenses = Object.keys(groups);
if (licenses.length === 0) {
  fail("pnpm reported no production dependency licenses");
}

const violations = [];
for (const license of licenses.sort()) {
  const packages = groups[license];
  if (!Array.isArray(packages) || packages.length === 0) {
    violations.push(`${license || "<empty>"}: invalid or empty package list`);
    continue;
  }

  for (const dependency of packages) {
    const validName = dependency && typeof dependency.name === "string" && dependency.name !== "";
    const name = validName ? dependency.name : "<unknown package>";
    const versions = dependency && Array.isArray(dependency.versions)
      ? dependency.versions.filter((version) => typeof version === "string" && version !== "")
      : [];
    const declared = dependency && typeof dependency.license === "string"
      ? dependency.license
      : "";

    if (
      !validName ||
      !allowed.has(license) ||
      !allowed.has(declared) ||
      declared !== license ||
      versions.length === 0
    ) {
      const versionText = versions.length === 0 ? "<unknown version>" : versions.join(",");
      const licenseText = declared === license
        ? declared
        : `${declared || "<unknown license>"} (grouped as ${license || "<unknown license>"})`;
      violations.push(`${name}@${versionText}: ${licenseText}`);
    }
  }
}

if (violations.length !== 0) {
  fail(`production dependency license violations:\n  ${violations.join("\n  ")}`);
}

console.log(`PASS: ${workspace} production dependencies use the JavaScript license allowlist`);
NODE
done
