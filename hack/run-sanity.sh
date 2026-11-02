#!/usr/bin/env bash
#
# Run the official csi-sanity suite against the real driver binary (D-31).
#
# The run has three parts: a GlusterFS server container with a single brick volume,
# the runner image (the published driver image plus the csi-sanity binary) and one
# docker network that connects the two. csi-sanity runs inside the runner container,
# next to the driver it tests, because both have to see the same staging and target
# directories.
#
# Every argument of this script is passed on to csi-sanity inside the container, so a
# single spec can be re-run with, for example:
#   hack/run-sanity.sh --ginkgo.focus='maximum-length'
#
# Environment:
#   DOCKER         docker command (default: docker)
#   GO_IMAGE       Go image of the driver and csi-sanity builds; passed to the image
#                  builds as --build-arg when set, so a local run can use a registry
#                  mirror. No mirror is named here.
#   BASE_IMAGE     base image of the driver and gluster images; same handling.
#   GOPROXY        Go module proxy for the csi-sanity build; passed as --build-arg
#                  when set. No proxy address is named here.
#   DRIVER_IMAGE   driver image to test (for example
#                  ghcr.io/gitlayzer/glusterfs-csi:sha-xxxxxxx). When unset, the
#                  repository Dockerfile is built as glusterfs-csi:sanity.
#   OUT_DIR        directory of the artifacts (default: _output/sanity)
#   KEEP=1         keep the containers, the network and the brick volume after the
#                  run and print their names, for debugging
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

DOCKER="${DOCKER:-docker}"
# GO_IMAGE, BASE_IMAGE, GOPROXY and DRIVER_IMAGE are deliberately unset by default:
# only a caller that sets them names a mirror, a proxy or an existing image.
GO_IMAGE="${GO_IMAGE:-}"
BASE_IMAGE="${BASE_IMAGE:-}"
GOPROXY="${GOPROXY:-}"
DRIVER_IMAGE="${DRIVER_IMAGE:-}"
OUT_DIR="${OUT_DIR:-_output/sanity}"
KEEP="${KEEP:-0}"

# The image names the runner Dockerfile refers to; the gluster image is only ever
# used locally.
readonly GLUSTER_IMAGE="glusterfs-csi-sanity-gluster:local"
readonly RUNNER_IMAGE="glusterfs-csi-sanity-runner:local"
readonly LOCAL_DRIVER_IMAGE="glusterfs-csi:sanity"

# RUN_ID keeps two runs on one host apart; PID alone is enough for that.
RUN_ID="${RUN_ID:-$$}"
readonly prefix="glusterfs-csi-sanity-${RUN_ID}"
readonly NETWORK="${prefix}-net"
readonly GLUSTER_CONTAINER="${prefix}-gluster"
readonly RUNNER_CONTAINER="${prefix}-runner"
# gluster_server_name is the host name of the gluster container. It is what
# test/sanity/config.json lists as the server and what the brick of the parent
# volume is named after, so the two have to agree.
readonly gluster_server_name="gluster-server"
readonly parent_volume="csi-parent"

# Resolve OUT_DIR before anything changes the working directory.
mkdir -p "${OUT_DIR}"
mkdir -p "$(dirname "${OUT_DIR}")"
OUT_DIR="$(cd "${OUT_DIR}" && pwd)"
readonly OUT_DIR

# shellcheck disable=SC2329 # cleanup is called by the EXIT trap below.
cleanup() {
  local status=$?

  if [[ "${KEEP}" == "1" ]]; then
    echo "KEEP=1: leaving the resources in place" >&2
    printf '  network:   %s\n  container: %s\n  container: %s\n' \
      "${NETWORK}" "${GLUSTER_CONTAINER}" "${RUNNER_CONTAINER}" >&2
    return 0
  fi

  # Cleanup never changes the exit status of the script: a container that is already
  # gone is not a failed run.
  "${DOCKER}" rm -f -v "${RUNNER_CONTAINER}" >/dev/null 2>&1 || true
  "${DOCKER}" rm -f -v "${GLUSTER_CONTAINER}" >/dev/null 2>&1 || true
  "${DOCKER}" network rm "${NETWORK}" >/dev/null 2>&1 || true

  return "${status}"
}
trap cleanup EXIT

# build_args holds the optional --build-arg flags of the current build.
build_args=()

# collect_build_args collects the --build-arg flags for the named variables. A value
# the caller did not set is not passed at all, so the default of the Dockerfile
# applies and nothing in this script names a registry or a proxy.
collect_build_args() {
  build_args=()

  local name value
  for name in "$@"; do
    value="${!name:-}"
    if [[ -n "${value}" ]]; then
      build_args+=(--build-arg "${name}=${value}")
    fi
  done
}

# run_build runs "docker build" with the collected --build-arg flags in front of the
# arguments of the call. The ${build_args[@]+...} form is what keeps an empty flag
# list from tripping "set -u": older bash releases treat an empty array as unset.
run_build() {
  "${DOCKER}" build ${build_args[@]+"${build_args[@]}"} "$@"
}

# Empty the artifact directory of a previous run, so that a PASS cannot come with
# the logs of an older FAIL next to it.
find "${OUT_DIR}" -mindepth 1 -maxdepth 1 -exec rm -rf {} +

# --- images ----------------------------------------------------------------
if [[ -n "${DRIVER_IMAGE}" ]]; then
  echo "==> using the driver image ${DRIVER_IMAGE}"
  driver_image="${DRIVER_IMAGE}"
else
  driver_image="${LOCAL_DRIVER_IMAGE}"
  echo "==> building ${driver_image}"

  # The version information is taken the same way the Makefile takes it, so the
  # binary under test reports the same version as a released one.
  version="$(git describe --tags --always --dirty 2>/dev/null || echo v0.0.0-dev)"
  git_commit="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
  build_date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

  collect_build_args GO_IMAGE BASE_IMAGE
  run_build \
    --build-arg "VERSION=${version}" \
    --build-arg "GIT_COMMIT=${git_commit}" \
    --build-arg "BUILD_DATE=${build_date}" \
    -t "${driver_image}" \
    .
fi

echo "==> building ${GLUSTER_IMAGE}"
collect_build_args BASE_IMAGE
run_build \
  -f test/sanity/gluster.Dockerfile \
  -t "${GLUSTER_IMAGE}" \
  test/sanity

echo "==> building ${RUNNER_IMAGE}"
collect_build_args GO_IMAGE GOPROXY
run_build \
  --build-arg "DRIVER_IMAGE=${driver_image}" \
  -f test/sanity/runner.Dockerfile \
  -t "${RUNNER_IMAGE}" \
  test/sanity

# --- network and backend ---------------------------------------------------
echo "==> creating the network ${NETWORK}"
"${DOCKER}" network create "${NETWORK}" >/dev/null

# The brick is a docker volume and not a directory of the container: gluster needs
# trusted extended attributes on it, which the overlay root file system of a
# container does not support.
echo "==> starting the gluster server ${GLUSTER_CONTAINER}"
"${DOCKER}" run -d --privileged \
  --name "${GLUSTER_CONTAINER}" \
  --hostname "${gluster_server_name}" \
  --network "${NETWORK}" \
  --network-alias "${gluster_server_name}" \
  --mount type=volume,dst=/bricks \
  "${GLUSTER_IMAGE}" >/dev/null

# The gluster CLI is only judged by its exit status; its human readable output is
# never parsed.
echo "==> waiting for glusterd"
deadline=$((SECONDS + 60))
until "${DOCKER}" exec "${GLUSTER_CONTAINER}" gluster --mode=script pool list >/dev/null 2>&1; do
  if ((SECONDS >= deadline)); then
    echo "glusterd did not become ready within 60s" >&2
    "${DOCKER}" logs "${GLUSTER_CONTAINER}" >&2 || true
    exit 1
  fi
  sleep 1
done

# The parent volume of the subdirectory volumes csi-sanity creates: one brick on the
# single gluster node, forced because the brick directory is not a mount point of its
# own.
echo "==> creating the parent volume ${parent_volume}"
if ! "${DOCKER}" exec "${GLUSTER_CONTAINER}" \
  gluster --mode=script volume create "${parent_volume}" \
  "${gluster_server_name}:/bricks/${parent_volume}" force; then
  echo "could not create the parent volume ${parent_volume}" >&2
  "${DOCKER}" logs "${GLUSTER_CONTAINER}" >&2 || true
  exit 1
fi
if ! "${DOCKER}" exec "${GLUSTER_CONTAINER}" \
  gluster --mode=script volume start "${parent_volume}"; then
  echo "could not start the parent volume ${parent_volume}" >&2
  "${DOCKER}" logs "${GLUSTER_CONTAINER}" >&2 || true
  exit 1
fi

echo "==> waiting for the parent volume to be online"
deadline=$((SECONDS + 30))
until "${DOCKER}" exec "${GLUSTER_CONTAINER}" \
  gluster --mode=script volume status "${parent_volume}" >/dev/null 2>&1; do
  if ((SECONDS >= deadline)); then
    echo "the parent volume did not become ready within 30s" >&2
    "${DOCKER}" logs "${GLUSTER_CONTAINER}" >&2 || true
    exit 1
  fi
  sleep 1
done

# --- the run ---------------------------------------------------------------
# csi-sanity runs next to the driver, in one privileged container: it creates the
# staging and target directories the driver then mounts, so both have to see the
# same file system. --rm keeps the container out of the way of the cleanup trap
# unless KEEP asked for it.
run=(
  run --privileged
  --network "${NETWORK}"
  --name "${RUNNER_CONTAINER}"
  --volume "${OUT_DIR}:/out"
)
if [[ "${KEEP}" != "1" ]]; then
  run+=(--rm)
fi

echo "==> running csi-sanity"
rc=0
"${DOCKER}" "${run[@]}" "${RUNNER_IMAGE}" "$@" || rc=$?

# The server log explains a failure on the backend side, for example a brick that
# refused a trusted extended attribute.
if ((rc != 0)); then
  if ! "${DOCKER}" cp "${GLUSTER_CONTAINER}:/var/log/glusterfs" "${OUT_DIR}/gluster-logs"; then
    echo "warning: could not copy the gluster logs out of ${GLUSTER_CONTAINER}" >&2
  fi
else
  "${DOCKER}" cp "${GLUSTER_CONTAINER}:/var/log/glusterfs" "${OUT_DIR}/gluster-logs" 2>/dev/null || true
fi

if ((rc == 0)); then
  echo "csi-sanity: PASS (artifacts in ${OUT_DIR})"
else
  echo "csi-sanity: FAIL (artifacts in ${OUT_DIR})" >&2
fi
exit "${rc}"
