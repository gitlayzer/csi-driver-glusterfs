#!/usr/bin/env bash
#
# Verify the kustomize deployment manifests: render the base, the sample overlay
# and the static example with "kubectl kustomize", schema-validate the result
# with kubeconform, and check that every rendered image reference carries an
# explicit tag that is not "latest".
#
# Environment:
#   KUBECTL       renderer (default: kubectl)
#   KUBECONFORM   validator, may carry arguments, for example
#                 "docker run --rm -i ghcr.io/yannh/kubeconform:v0.8.0"
#                 (default: kubeconform)
#   K8S_VERSION   Kubernetes version to validate against (default: 1.28.0)
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

KUBECTL="${KUBECTL:-kubectl}"
KUBECONFORM="${KUBECONFORM:-kubeconform}"
K8S_VERSION="${K8S_VERSION:-1.28.0}"

# KUBECONFORM may be a whole command line (the docker image invocation). Split it
# on whitespace with read -r -a instead of eval, so the shell never re-parses it.
read -r -a kubeconform <<<"${KUBECONFORM}"

dirs=(base overlays/sample examples/static)

rendered="$(mktemp)"
trap 'rm -f "${rendered}"' EXIT

# check_images rejects every image reference that is not pinned to a tag, as
# well as the "latest" tag. A digest-only reference (repo@sha256:...) is
# rejected too: these manifests are expected to name a tag.
check_images() {
  local file="$1"
  local line image last tag checked=0

  while IFS= read -r line; do
    # Strip the "image:" key, surrounding whitespace and optional quotes.
    image="${line#*image:}"
    image="$(printf '%s' "${image}" | sed -e 's/^[[:space:]]*//' -e "s/[[:space:]\"']*$//")"

    # A digest only counts when a tag is present as well.
    image="${image%%@*}"
    last="${image##*/}"
    if [[ "${last}" != *:* ]]; then
      echo "error: image \"${image}\" has no explicit tag" >&2
      return 1
    fi
    tag="${last##*:}"
    if [[ -z "${tag}" ]]; then
      echo "error: image \"${image}\" has an empty tag" >&2
      return 1
    fi
    if [[ "${tag}" == "latest" ]]; then
      echo "error: image \"${image}\" uses the \"latest\" tag" >&2
      return 1
    fi
    checked=$((checked + 1))
  done < <(grep -E '^[[:space:]]*(- )?image:[[:space:]]*[^[:space:]]' "${file}" || true)

  echo "    ${checked} image reference(s) tagged explicitly"
}

for dir in "${dirs[@]}"; do
  echo "==> ${dir}"
  "${KUBECTL}" kustomize "deploy/kustomize/${dir}" >"${rendered}"
  "${kubeconform[@]}" -strict -summary -kubernetes-version "${K8S_VERSION}" - <"${rendered}"
  check_images "${rendered}"
done

echo "Manifests verified against Kubernetes ${K8S_VERSION}."
