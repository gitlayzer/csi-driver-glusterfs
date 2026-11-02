# syntax=docker/dockerfile:1

# GlusterFS server for the csi-sanity run (D-31).
#
# This image exists only for hack/run-sanity.sh: a single node with a single brick
# volume, which csi-sanity uses as the parent volume of the subdirectory volumes it
# creates. It is not a production image - it has no volume management, no quorum,
# no backup and no upgrade path.
#
# BASE_IMAGE is overridable so that a local run can point at a registry mirror, for
# example --build-arg BASE_IMAGE=<mirror>/library/debian:trixie-slim
ARG BASE_IMAGE=debian:trixie-slim
FROM ${BASE_IMAGE}

# glusterfs-server ships glusterd and the gluster CLI; --no-install-recommends keeps
# the image small and the apt lists are removed in the same layer.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      glusterfs-server \
 && rm -rf /var/lib/apt/lists/*

# -N keeps glusterd in the foreground, so the container and glusterd share one
# lifetime: docker stop sends SIGTERM to glusterd itself instead of leaving an
# orphaned daemon behind.
ENTRYPOINT ["/usr/sbin/glusterd", "-N", "--log-level=INFO"]
