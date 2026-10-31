# csi-driver-glusterfs

[![CI](https://github.com/gitlayzer/csi-driver-glusterfs/actions/workflows/ci.yml/badge.svg)](https://github.com/gitlayzer/csi-driver-glusterfs/actions/workflows/ci.yml)
[![Image](https://github.com/gitlayzer/csi-driver-glusterfs/actions/workflows/image.yml/badge.svg)](https://github.com/gitlayzer/csi-driver-glusterfs/actions/workflows/image.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

A Container Storage Interface (CSI) driver that lets Kubernetes consume GlusterFS volumes,
built on native glusterd and the gluster CLI only — no Heketi, no GlusterD2, no extra database.

Status: **M0 in progress** — repository scaffold and CI are in place; no CSI functionality yet.

- [Architecture](docs/architecture.md)
- [Roadmap & task breakdown](docs/roadmap.md)
- [Usage (target)](docs/usage.md)
- [Decisions](docs/decisions.md)
- [Rules for coding agents](AGENTS.md)

Provisioning modes: `static` (existing volume), `subdir` (directory per PVC in a shared volume),
`volume` (dedicated replica volume per PVC, with snapshots on LVM-thin bricks).

## Contributing

Contributions are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md) for the development
setup, the Conventional Commits convention and the required DCO sign-off (`git commit -s`),
and [AGENTS.md](AGENTS.md) for the rules every change must follow.
