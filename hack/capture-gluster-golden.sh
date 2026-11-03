#!/usr/bin/env bash
#
# Capture the golden samples of pkg/gluster (T-16, D-35): the real output of
# `gluster --mode=script --xml` for every command the client wraps, on the GlusterFS
# releases packaged by Debian bullseye (9.2), bookworm (10.3) and trixie (11.1).
#
# Each release runs in one privileged single-node glusterd container whose host name
# is gfs-golden. Every sample is saved as <name>.xml (stdout), <name>.stderr and
# <name>.exit under pkg/gluster/testdata/gluster-<version>/, together with
# version.txt. glusterd-down comes from a second container of the same image in which
# glusterd is not running at all.
#
# Usage: hack/capture-gluster-golden.sh [debian release ...]   (default: all three)
#
# Environment:
#   DOCKER        docker command (default: docker)
#   DEBIAN_IMAGE  repository of the Debian base image (default: debian); a local run
#                 may point it at a registry mirror. No mirror is named here.
#
# Volume ids, peer ids, ports and pids differ on every run; the tests do not depend
# on their values.
set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DOCKER="${DOCKER:-docker}"
DEBIAN_IMAGE="${DEBIAN_IMAGE:-debian}"
OUT="${repo_root}/pkg/gluster/testdata"

# capture NAME ARGS...: run gluster in the container and keep stdout, stderr and the
# exit status.
capture() {
  local name=$1
  shift
  "${DOCKER}" exec "${container}" gluster --mode=script --xml "$@" >"${dir}/${name}.xml" 2>"${dir}/${name}.stderr"
  echo $? >"${dir}/${name}.exit"
  printf '  %-34s exit=%s %s\n' "${name}" "$(cat "${dir}/${name}.exit")" \
    "$(grep -o '<opRet>[^<]*</opRet>' "${dir}/${name}.xml" | head -1)"
}
sh_in() { "${DOCKER}" exec "${container}" sh -c "$1"; }

releases=("$@")
[ ${#releases[@]} -gt 0 ] || releases=(bullseye bookworm trixie)
for release in "${releases[@]}"; do
  echo "################ ${release}"
  image="glusterfs-csi-golden:${release}"
  container="glusterfs-csi-golden-${release}"
  "${DOCKER}" rm -f "${container}" >/dev/null 2>&1
  # bullseye's security suite is no longer served by every mirror; the gluster
  # packages come from the main suite anyway.
  "${DOCKER}" build -q -t "${image}" - <<EOF || { echo "build failed: ${release}"; continue; }
FROM ${DEBIAN_IMAGE}:${release}-slim
RUN if [ ${release} = bullseye ]; then sed -i '/security/d' /etc/apt/sources.list; fi \
 && apt-get update \
 && apt-get install -y --no-install-recommends glusterfs-server glusterfs-client procps \
 && rm -rf /var/lib/apt/lists/*
ENTRYPOINT ["/usr/sbin/glusterd", "-N", "--log-level=INFO"]
EOF
  "${DOCKER}" run -d --privileged --hostname gfs-golden --name "${container}" "${image}" >/dev/null
  for _ in $(seq 1 30); do
    "${DOCKER}" exec "${container}" gluster --mode=script volume list >/dev/null 2>&1 && break
    sleep 1
  done
  version="$("${DOCKER}" exec "${container}" gluster --version | head -1 | awk '{print $2}')"
  dir="${OUT}/gluster-${version}"
  rm -rf "${dir}"
  mkdir -p "${dir}"
  echo "glusterfs ${version}" >"${dir}/version.txt"
  sh_in 'mkdir -p /bricks/b1 /bricks/b2 /bricks/b3 /bricks/one /mnt/t16'

  "${DOCKER}" run --rm --entrypoint gluster "${image}" --mode=script --xml volume info \
    >"${dir}/glusterd-down.xml" 2>"${dir}/glusterd-down.stderr"
  echo $? >"${dir}/glusterd-down.exit"

  capture volume-info-none        volume info
  capture volume-info-missing     volume info nosuch
  capture volume-create-replica3  volume create t16vol replica 3 gfs-golden:/bricks/b1/t16vol gfs-golden:/bricks/b2/t16vol gfs-golden:/bricks/b3/t16vol force
  capture volume-create-exists    volume create t16vol replica 3 gfs-golden:/bricks/b1/t16vol gfs-golden:/bricks/b2/t16vol gfs-golden:/bricks/b3/t16vol force
  capture volume-create-bad-brick volume create t16bad gfs-golden:/bricks/b1/t16vol force
  capture volume-info-created     volume info t16vol
  capture volume-status-not-started volume status t16vol
  capture volume-start            volume start t16vol
  capture volume-start-again      volume start t16vol
  capture volume-start-missing    volume start nosuch
  capture volume-info-started     volume info t16vol
  capture volume-create-single    volume create t16one gfs-golden:/bricks/one/t16one force
  capture volume-start-single     volume start t16one
  capture volume-info-all         volume info
  capture volume-status           volume status t16vol
  capture volume-status-detail    volume status t16vol detail
  capture volume-status-missing   volume status nosuch
  capture volume-set              volume set t16vol performance.cache-size 64MB
  capture volume-set-bad-option   volume set t16vol no.such.option x
  capture quota-limit-not-enabled volume quota t16vol limit-usage /d1 1073741824
  capture quota-list-not-enabled  volume quota t16vol list
  capture quota-enable            volume quota t16vol enable
  capture quota-enable-again      volume quota t16vol enable
  capture quota-enable-missing    volume quota nosuch enable
  sh_in 'mount -t glusterfs gfs-golden:/t16vol /mnt/t16 && mkdir -p /mnt/t16/d1 /mnt/t16/d2 /mnt/t16/d3 && dd if=/dev/zero of=/mnt/t16/d1/f bs=1M count=5 2>/dev/null'
  capture quota-list-no-limits    volume quota t16vol list
  capture quota-limit-missing-dir volume quota t16vol limit-usage /nosuch 1073741824
  capture quota-limit             volume quota t16vol limit-usage /d1 1073741824
  capture quota-limit-units       volume quota t16vol limit-usage /d2 2GB
  capture quota-limit-again       volume quota t16vol limit-usage /d1 2147483648
  capture quota-limit-bad-size    volume quota t16vol limit-usage /d3 notasize
  capture quota-limit-root        volume quota t16vol limit-usage / 10GB
  sleep 12 # let quotad account for the 5 MiB in d1
  capture quota-list-path         volume quota t16vol list /d1
  capture quota-list-paths        volume quota t16vol list /d1 /d2
  capture quota-list-all          volume quota t16vol list
  capture quota-list-missing-path volume quota t16vol list /nosuch
  capture quota-list-no-limit-path volume quota t16vol list /d3
  capture quota-remove            volume quota t16vol remove /d2
  capture quota-remove-again      volume quota t16vol remove /d2
  capture quota-remove-missing-dir volume quota t16vol remove /nosuch
  sh_in 'umount /mnt/t16'
  capture volume-stop             volume stop t16vol
  capture volume-stop-again       volume stop t16vol
  capture volume-stop-missing     volume stop nosuch
  capture volume-status-stopped   volume status t16vol
  capture volume-delete-started   volume delete t16one
  capture volume-delete           volume delete t16vol
  capture volume-delete-missing   volume delete t16vol
  capture volume-info-after-delete volume info
  "${DOCKER}" rm -f "${container}" >/dev/null
done
