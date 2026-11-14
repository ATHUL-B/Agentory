#!/usr/bin/env bash
# Delete the local dev cluster. Generated secrets are kept unless PURGE=1.
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require_cmd kind
if cluster_exists; then
  log "Deleting kind cluster '${KIND_CLUSTER_NAME}'"
  kind delete cluster --name "${KIND_CLUSTER_NAME}"
  ok "deleted"
else
  ok "cluster '${KIND_CLUSTER_NAME}' does not exist"
fi

if [[ "${PURGE:-0}" == "1" ]]; then
  rm -rf "${SECRETS_DIR}"
  ok "removed ${SECRETS_DIR}"
fi
