#!/usr/bin/env bash
#
# Verify the Helm chart deploy/helm/glusterfs-csi: lint it, check that it fails
# to render without clusters, render it with the sample values into a Kubernetes
# 1.28 schema, and compare the result byte for byte with the kustomize sample
# overlay (D-33). The kustomize manifests are the baseline: if the two disagree,
# the chart is wrong.
#
# Environment:
#   HELM          helm binary (default: helm)
#   KUBECTL       renderer of the kustomize side (default: kubectl)
#   KUBECONFORM   validator, may carry arguments, for example
#                 "docker run --rm -i ghcr.io/yannh/kubeconform:v0.8.0"
#                 (default: kubeconform)
#   YQ            yq v4 binary, may carry arguments, for example
#                 "docker run --rm -i mikefarah/yq:4.54.1"
#                 (default: docker run --rm -i mikefarah/yq:4.54.1)
#   K8S_VERSION   Kubernetes version to render and validate against
#                 (default: 1.28.0)
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

HELM="${HELM:-helm}"
KUBECTL="${KUBECTL:-kubectl}"
KUBECONFORM="${KUBECONFORM:-kubeconform}"
YQ="${YQ:-docker run --rm -i mikefarah/yq:4.54.1}"
K8S_VERSION="${K8S_VERSION:-1.28.0}"

chart="deploy/helm/glusterfs-csi"
sample_values="${chart}/ci/sample-values.yaml"

# KUBECONFORM and YQ may be whole command lines (docker image invocations). Split
# them on whitespace with read -r -a instead of eval, so the shell never re-parses
# them and nothing in them is expanded or executed by the caller's shell.
read -r -a kubeconform <<<"${KUBECONFORM}"
read -r -a yq <<<"${YQ}"

# The normalisation applied to both sides before the diff. It drops null
# documents and the Namespace object (the chart has none), removes the Helm-only
# labels from the top-level metadata.labels only - a Helm label inside a Pod
# template therefore still shows up as a difference - compares config.json by
# JSON meaning rather than by text, and sorts documents by kind/name, then every
# mapping key, and drops comments and quoting style. The expression is passed as
# a single argument to "yq ea", so it works with any yq binary or image and needs
# no volume mount.
normalise_expr='[.] | map(select(. != null and .kind != "Namespace"))
| map(del(.metadata.labels."helm.sh/chart", .metadata.labels."app.kubernetes.io/managed-by", .metadata.labels."app.kubernetes.io/instance", .metadata.labels."app.kubernetes.io/version"))
| map(with(select(.kind == "ConfigMap" and .data."config.json"); .data."config.json" |= (from_json | sort_keys(..) | to_json(0))))
| sort_by(.kind, .metadata.name) | .[]
| (... comments="") | sort_keys(..) | (... style="") | split_doc'

# The StorageClass fields compared one by one with the dynamic provisioning
# example; the rest of the object (labels) is installer territory.
storageclass_expr='[.] | map(select(.kind == "StorageClass" and .metadata.name == "glusterfs-subdir"))
 | map({"provisioner": .provisioner, "parameters": .parameters, "reclaimPolicy": .reclaimPolicy, "volumeBindingMode": .volumeBindingMode, "allowVolumeExpansion": .allowVolumeExpansion})
 | sort_keys(..) | .[]'

tmpdir="$(mktemp -d)"
trap 'rm -rf "${tmpdir}"' EXIT

rendered="${tmpdir}/helm.yaml"
rendered_no_sc="${tmpdir}/helm-no-storageclass.yaml"
rendered_helm_norm="${tmpdir}/helm.norm.yaml"
rendered_kustomize_norm="${tmpdir}/kustomize.norm.yaml"
kubelet_rendered="${tmpdir}/helm-kubelet-dir.yaml"

# expect_render_failure runs "helm template" and requires it to fail with a
# message that mentions the given string. --kube-version is always passed: Helm v4
# otherwise renders against its own default version, which is older than the
# kubeVersion of the chart and would hide the error under test.
expect_render_failure() {
  local description="$1"
  local needle="$2"
  shift 2

  local out status=0
  out="$("${HELM}" template glusterfs-csi "${chart}" \
    --kube-version "${K8S_VERSION}" "$@" 2>&1)" || status=$?

  if [[ "${status}" -eq 0 ]]; then
    echo "error: rendering must fail (${description}) but succeeded" >&2
    return 1
  fi
  if [[ "${out}" != *"${needle}"* ]]; then
    echo "error: rendering failed (${description}) without mentioning \"${needle}\":" >&2
    echo "${out}" >&2
    return 1
  fi
  echo "    failed as expected: ${description}"
}

echo "==> Helm version"
"${HELM}" version --short

echo "==> Lint"
"${HELM}" lint --strict "${chart}" -f "${sample_values}"

echo "==> Values that must be rejected"
expect_render_failure "default values, no clusters" "clusters"
expect_render_failure "clusters entry without clusterID" "clusterID" \
  --set 'clusters[0].servers={gluster-1.example.com}'

echo "==> Render"
"${HELM}" template glusterfs-csi "${chart}" \
  --namespace glusterfs-csi \
  --kube-version "${K8S_VERSION}" \
  -f "${sample_values}" >"${rendered}"

echo "==> kubeconform"
"${kubeconform[@]}" -strict -summary -kubernetes-version "${K8S_VERSION}" - <"${rendered}"

echo "==> Compare with the kustomize sample overlay"
"${KUBECTL}" kustomize deploy/kustomize/overlays/sample >"${tmpdir}/kustomize.yaml"
# The StorageClass is compared separately below: it comes from the dynamic
# provisioning example, not from the sample overlay.
"${yq[@]}" ea 'select(.kind != "StorageClass")' - <"${rendered}" >"${rendered_no_sc}"
"${yq[@]}" ea "${normalise_expr}" - <"${rendered_no_sc}" >"${rendered_helm_norm}"
"${yq[@]}" ea "${normalise_expr}" - <"${tmpdir}/kustomize.yaml" >"${rendered_kustomize_norm}"
if ! diff -u "${rendered_kustomize_norm}" "${rendered_helm_norm}"; then
  cat >&2 <<'EOF'
error: the default rendering of the chart differs from the kustomize sample
overlay (left: kustomize, right: helm). Fix the chart, never deploy/kustomize/.
EOF
  exit 1
fi
echo "    identical"

echo "==> Compare the StorageClass with the dynamic provisioning example"
"${KUBECTL}" kustomize deploy/kustomize/examples/dynamic >"${tmpdir}/dynamic.yaml"
"${yq[@]}" ea "${storageclass_expr}" - <"${rendered}" >"${tmpdir}/storageclass.helm.yaml"
"${yq[@]}" ea "${storageclass_expr}" - <"${tmpdir}/dynamic.yaml" >"${tmpdir}/storageclass.kustomize.yaml"
if [[ ! -s "${tmpdir}/storageclass.helm.yaml" ]]; then
  echo "error: the sample values did not render the StorageClass glusterfs-subdir" >&2
  exit 1
fi
if ! diff -u "${tmpdir}/storageclass.kustomize.yaml" "${tmpdir}/storageclass.helm.yaml"; then
  cat >&2 <<'EOF'
error: the StorageClass glusterfs-subdir rendered by the chart differs from
deploy/kustomize/examples/dynamic/storageclass.yaml (left: kustomize, right: helm).
EOF
  exit 1
fi
echo "    identical"

echo "==> kubeletDir"
"${HELM}" template glusterfs-csi "${chart}" \
  --namespace glusterfs-csi \
  --kube-version "${K8S_VERSION}" \
  -f "${sample_values}" \
  --set kubeletDir=/data/kubelet >"${kubelet_rendered}"
remaining="$(grep -c '/var/lib/kubelet' "${kubelet_rendered}" || true)"
if [[ "${remaining}" -ne 0 ]]; then
  echo "error: /var/lib/kubelet still appears ${remaining} time(s) with kubeletDir=/data/kubelet:" >&2
  grep -n '/var/lib/kubelet' "${kubelet_rendered}" >&2
  exit 1
fi
if ! grep -q '/data/kubelet' "${kubelet_rendered}"; then
  echo "error: /data/kubelet does not appear with kubeletDir=/data/kubelet" >&2
  exit 1
fi
"${kubeconform[@]}" -strict -summary -kubernetes-version "${K8S_VERSION}" - <"${kubelet_rendered}"

echo "Helm chart verified with $("${HELM}" version --short) against Kubernetes ${K8S_VERSION}."
