#!/usr/bin/env bash
# scripts/test/bootstrap-trap-safety.sh
#
# Regression guard: bootstrap.sh must not produce
#   "askpass_dir: unbound variable"
# when the script exits early (before ensure_repo() ever ran).
#
# Live VM 2026-05-22: clouddeployctl apply failed at OS gate before
# bootstrap's git-clone path was exercised; the EXIT trap then fired
# with `set -u` still on and `${askpass_dir}` was function-local and
# out of scope. Result was a confusing trailing
#   ./bootstrap.sh: line 1: askpass_dir: unbound variable
# overlaid on the real "OS unsupported" error.
#
# This script extracts the trap-installation block from bootstrap.sh,
# appends an early-exit stub, runs the synthesized script, and checks
# stderr for "unbound variable". CI invokes this from the shell-lint
# job alongside `bash -n bootstrap.sh`.

set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
bootstrap="${root}/bootstrap.sh"

if [[ ! -f "${bootstrap}" ]]; then
    echo "FAIL: bootstrap.sh not found at ${bootstrap}" >&2
    exit 1
fi

top="$(sed -n '/^set -euo pipefail/,/^trap cleanup_bootstrap EXIT/p' "${bootstrap}")"
if [[ -z "${top}" ]]; then
    echo "FAIL: could not extract trap-installation block from bootstrap.sh" >&2
    echo "      (looked for '^set -euo pipefail' .. '^trap cleanup_bootstrap EXIT')" >&2
    exit 1
fi

script="$(mktemp /tmp/bootstrap-trap-safety.XXXXXX.sh)"
trap 'rm -f "${script}"' EXIT
{
    echo "#!/usr/bin/env bash"
    echo "${top}"
    echo "# Simulate an early-exit failure path (apt failure, network down,"
    echo "# OS gate refusing the host, ...)."
    echo "echo 'synthetic early-exit failure' >&2"
    echo "exit 1"
} > "${script}"
chmod +x "${script}"

# Run with `bash` (NOT bash -e at our level) so we capture the exit
# code of the synthesized script + its stderr without aborting this
# test harness.
output="$(bash "${script}" 2>&1 || true)"

if grep -q "unbound variable" <<<"${output}"; then
    echo "FAIL: bootstrap.sh still produces 'unbound variable' on early exit." >&2
    echo "captured output:" >&2
    echo "${output}" >&2
    exit 1
fi

echo "OK: bootstrap.sh trap is safe under early-exit (no 'unbound variable')."
