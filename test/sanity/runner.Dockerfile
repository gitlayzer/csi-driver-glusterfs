# syntax=docker/dockerfile:1

# The csi-sanity runner image (D-31).
#
# The base image is the image the project publishes, so what is tested is the real
# driver binary at /glusterfs-csi together with the glusterfs-client of that image,
# not a locally built variant. On top of it go the official csi-sanity binary and
# the three files that drive the run.
#
# GO_IMAGE and DRIVER_IMAGE are overridable so that a local run can point at a
# registry mirror or at an already published driver image, for example
# --build-arg DRIVER_IMAGE=<registry>/<repo>:sha-xxxxxxx
ARG GO_IMAGE=golang:1.27
ARG DRIVER_IMAGE=glusterfs-csi:sanity

# --- csi-sanity ------------------------------------------------------------
# csi-sanity is built here and only here: it is a test tool of the runner image and
# deliberately not a dependency of the module (go.mod stays free of csi-test,
# ginkgo and gomega).
FROM ${GO_IMAGE} AS csi-sanity

# GOPROXY is passed through only when the caller sets it, which lets a run behind a
# proxy work without the build file naming any proxy address itself.
ARG GOPROXY
ARG CSI_SANITY_VERSION=v5.6.0

RUN CGO_ENABLED=0 GOBIN=/out go install \
      github.com/kubernetes-csi/csi-test/v5/cmd/csi-sanity@${CSI_SANITY_VERSION}

# --- runner ----------------------------------------------------------------
FROM ${DRIVER_IMAGE}

COPY --from=csi-sanity /out/csi-sanity /usr/local/bin/csi-sanity
COPY config.json parameters.yaml run-in-container.sh /sanity/

ENTRYPOINT ["/sanity/run-in-container.sh"]
