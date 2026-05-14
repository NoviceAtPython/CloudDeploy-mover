#!/usr/bin/env bash
set -euo pipefail

ENV_FILE="${CLOUDDEPLOY_USER_ENV_FILE:-${HOME}/.config/clouddeploy/env}"
UNIT_NAME="${CLOUDDEPLOY_SYSTEMD_UNIT:-clouddeploy-manual-rerun}"
INSTALL_OPTIONAL_APPS_VALUE="${INSTALL_OPTIONAL_APPS:-0}"

if [[ ! -f "${ENV_FILE}" ]]; then
        cat >&2 <<EOF
Missing ${ENV_FILE}

Create it with mode 0600, for example:
  mkdir -p "\${HOME}/.config/clouddeploy"
  chmod 700 "\${HOME}/.config/clouddeploy"
  install -m 0600 /dev/null "\${HOME}/.config/clouddeploy/env"
  editor "\${HOME}/.config/clouddeploy/env"

Required values are typically:
  SUNSHINE_PASS=...
  TAILSCALE_AUTHKEY=...
EOF
        exit 1
fi

chmod 600 "${ENV_FILE}" 2>/dev/null || true

exec systemd-run \
        --unit="${UNIT_NAME}" \
        --same-dir \
        --wait \
        --collect \
        --pty \
        -E CLOUDDEPLOY_USER_ENV_FILE="${ENV_FILE}" \
        -E INSTALL_OPTIONAL_APPS="${INSTALL_OPTIONAL_APPS_VALUE}" \
        /bin/bash -lc 'set -euo pipefail; set -a; source "${CLOUDDEPLOY_USER_ENV_FILE}"; set +a; export INSTALL_OPTIONAL_APPS="${INSTALL_OPTIONAL_APPS:-0}"; exec /bin/bash ./CloudDeploy-wayland.sh'
