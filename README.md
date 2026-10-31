# csi-driver-glusterfs

A Container Storage Interface (CSI) driver that lets Kubernetes consume GlusterFS volumes,
built on native glusterd and the gluster CLI only — no Heketi, no GlusterD2, no extra database.

Status: **design phase** (M0 not started).

- [Architecture](docs/architecture.md)
- [Roadmap & task breakdown](docs/roadmap.md)
- [Usage (target)](docs/usage.md)
- [Decisions](docs/decisions.md)
- [Rules for coding agents](AGENTS.md)

Provisioning modes: `static` (existing volume), `subdir` (directory per PVC in a shared volume),
`volume` (dedicated replica volume per PVC, with snapshots on LVM-thin bricks).
