#!/usr/bin/env bash
#
# Run csi-sanity against the driver binary inside the runner container (D-31).
#
# This is the entrypoint of test/sanity/runner.Dockerfile: it starts the driver in
# all mode on a unix socket, runs csi-sanity against it, checks that the run left
# nothing behind, stops the driver and reports the result. hack/run-sanity.sh
# prepares the gluster server and starts this container.
#
# Every argument of the script is appended to the csi-sanity command line, so a
# single spec can be re-run with, for example,
#   docker run ... <runner image> --ginkgo.focus='maximum-length'
#
# Nothing here parses human readable gluster output, and no success is decided by
# it: the driver and gluster are judged by exit codes and by the state of the file
# system and the mount namespace.
#
# Exit status: 0 when csi-sanity passed and every check passed, 1 otherwise.
set -euo pipefail

readonly OUT=/out
readonly SOCK=/csi/csi.sock
readonly PARENT=/var/lib/glusterfs-csi/controller/sanity/csi-parent
readonly STATE=/var/lib/glusterfs-csi/state
readonly MOUNT_DIR=/tmp/csi-mount
readonly STAGING_DIR=/tmp/csi-staging
readonly DRIVER=/glusterfs-csi
readonly MOUNT_LOG_DIR=/var/log/glusterfs/csi
# meta_dir is the hidden directory the driver keeps in a parent volume (D-25).
readonly meta_dir=".glusterfs-csi"

# driver_pid is the pid of the driver process, so that an early exit of this script
# never leaves it running.
driver_pid=""

# stop_driver_on_exit stops a still running driver. It is registered before the
# driver starts, because every early exit has to clean up as well.
# shellcheck disable=SC2329 # stop_driver_on_exit is called by the EXIT trap below.
stop_driver_on_exit() {
  if [[ -n "${driver_pid}" ]] && kill -0 "${driver_pid}" 2>/dev/null; then
    kill -TERM "${driver_pid}" 2>/dev/null || true
  fi
}
trap stop_driver_on_exit EXIT

# mount_points prints the mount point (fifth field) of every line of mountinfo.
mount_points() {
  awk '{ print $5 }' /proc/self/mountinfo
}

# mount_fs_types prints the mount point and the file system type of every line of
# mountinfo, separated by one space. The file system type is the first field after
# the " - " separator.
mount_fs_types() {
  awk '{
    fstype = ""
    for (i = 1; i < NF; i++) {
      if ($i == "-") {
        fstype = $(i + 1)
        break
      }
    }
    print $5, fstype
  }' /proc/self/mountinfo
}

# is_mount_point reports whether path is a mount point of this mount namespace.
is_mount_point() {
  local path="$1"
  awk -v target="${path}" '$5 == target { found = 1 } END { exit !found }' /proc/self/mountinfo
}

# report_leftovers prints a LEFTOVER line for every entry below dir, one level deep,
# except dir itself. A missing dir has no entries.
report_leftovers() {
  local dir="$1"
  local what="$2"
  local entry

  [[ -d "${dir}" ]] || return 0

  while IFS= read -r -d '' entry; do
    echo "LEFTOVER: ${entry#/} is left in the ${what}" >&2
    rc=1
  done < <(find "${dir}" -mindepth 1 -maxdepth 1 -print0 2>/dev/null || true)
}

# rc is the exit code the script ends with: the csi-sanity result, lowered to 1 by a
# failed check. It is set before the checks so that they can report into it.
rc=0

mkdir -p /csi "${OUT}" "${STATE}" "${MOUNT_LOG_DIR}"

# The driver runs with the defaults of the published image except for the paths and
# the log level: --recovery-interval is left at its default so that the mount
# recovery of a real deployment runs during the test as well.
"${DRIVER}" \
  --mode=all \
  --endpoint="unix://${SOCK}" \
  --nodeid=sanity-node \
  --config=/sanity/config.json \
  --controller-mount-dir=/var/lib/glusterfs-csi/controller \
  --state-dir="${STATE}" \
  --mount-log-dir="${MOUNT_LOG_DIR}" \
  --v=4 \
  >"${OUT}/driver.log" 2>&1 &
driver_pid="$!"

# Wait for the socket, at most 30s. A driver that exits during the wait is a failed
# start, not a slow one.
deadline=$((SECONDS + 30))
while [[ ! -S "${SOCK}" ]]; do
  if ! kill -0 "${driver_pid}" 2>/dev/null; then
    echo "the driver exited before it created ${SOCK}" >&2
    tail -n 50 "${OUT}/driver.log" >&2 || true
    exit 1
  fi
  if ((SECONDS >= deadline)); then
    echo "timed out after 30s waiting for ${SOCK}" >&2
    tail -n 50 "${OUT}/driver.log" >&2 || true
    kill -KILL "${driver_pid}" 2>/dev/null || true
    exit 1
  fi
  sleep 1
done

# csi-sanity writes the JUnit report and the log; its own output stays on stdout so
# that a failing spec is visible in the CI log even before the artifact is opened.
sanity_args=(
  --csi.endpoint="unix://${SOCK}"
  --csi.mountdir="${MOUNT_DIR}"
  --csi.stagingdir="${STAGING_DIR}"
  --csi.testvolumeparameters=/sanity/parameters.yaml
  --csi.testvolumesize=1073741824
  --ginkgo.junit-report="${OUT}/junit.xml"
  --ginkgo.no-color
)
csi-sanity "${sanity_args[@]}" "$@" || rc=$?

# The checks run while the driver is still up: the parent volume is mounted by the
# driver, so its content can only be inspected now, and the state directory is
# removed by the driver itself.
if ! is_mount_point "${PARENT}"; then
  echo "CHECK FAILED: parent volume is not mounted, cannot check for leftovers" >&2
  rc=1
else
  # Every volume csi-sanity created and deleted is gone again, and only the hidden
  # metadata directory of the driver is left at the top of the parent volume.
  while IFS= read -r -d '' entry; do
    name="${entry#"${PARENT}"/}"
    if [[ "${name}" == "${meta_dir}" ]]; then
      continue
    fi
    echo "LEFTOVER: ${name} is left in the parent volume" >&2
    rc=1
  done < <(find "${PARENT}" -mindepth 1 -maxdepth 1 -print0 2>/dev/null || true)

  # Neither the metadata of a volume nor an archived directory may survive: with
  # onDelete "delete" every DeleteVolume removes both.
  report_leftovers "${PARENT}/${meta_dir}/volumes" "metadata directory"
  report_leftovers "${PARENT}/${meta_dir}/archive" "archive directory"
fi

# No mount of the volume directories may survive a NodeUnpublishVolume or a
# NodeUnstageVolume, neither at the staging path nor at a target path.
while IFS= read -r target; do
  for dir in "${MOUNT_DIR}" "${STAGING_DIR}"; do
    if [[ "${target}" == "${dir}" || "${target}" == "${dir}/"* ]]; then
      echo "LEFTOVER: ${target} is still mounted" >&2
      rc=1
    fi
  done
done < <(mount_points)

# Every NodeUnstageVolume removes its staging record, so the state directory is
# empty after a complete run.
report_leftovers "${STATE}" "state directory"

# A panic or a fatal error in the driver log is a crash even if every spec passed.
if grep -q -e 'panic:' -e 'fatal error:' "${OUT}/driver.log"; then
  echo "CHECK FAILED: the driver log contains a panic or a fatal error" >&2
  rc=1
fi

# The driver has to stop cleanly on SIGTERM: a non-zero exit means the shutdown
# path, which unmounts everything, did not finish.
kill -TERM "${driver_pid}" 2>/dev/null || true
deadline=$((SECONDS + 30))
timed_out=0
while kill -0 "${driver_pid}" 2>/dev/null; do
  if ((SECONDS >= deadline)); then
    echo "CHECK FAILED: the driver did not exit within 30s of SIGTERM" >&2
    kill -KILL "${driver_pid}" 2>/dev/null || true
    timed_out=1
    rc=1
    break
  fi
  sleep 1
done
if ((timed_out == 0)); then
  driver_rc=0
  wait "${driver_pid}" || driver_rc=$?
  driver_pid=""
  if ((driver_rc != 0)); then
    echo "CHECK FAILED: the driver exited with status ${driver_rc}, want 0" >&2
    rc=1
  fi
fi

# After a clean shutdown no glusterfs mount of this container may be left: a
# remaining one is a leaked FUSE process that would keep the node plugin alive.
while IFS= read -r line; do
  if [[ "${line##* }" == "fuse.glusterfs" ]]; then
    echo "CHECK FAILED: glusterfs mounts left after shutdown: ${line% *}" >&2
    rc=1
  fi
done < <(mount_fs_types)

# The artifacts belong to root inside the container; without the chmod the runner
# user of a Linux CI host cannot remove or read them from the mounted directory.
mkdir -p "${OUT}/mount-logs"
if ! cp -a "${MOUNT_LOG_DIR}/." "${OUT}/mount-logs/"; then
  echo "warning: could not copy ${MOUNT_LOG_DIR} into ${OUT}/mount-logs" >&2
fi
chmod -R a+rwX "${OUT}"

if ((rc == 0)); then
  echo "csi-sanity: PASS"
else
  echo "csi-sanity: FAIL"
fi
exit "${rc}"
