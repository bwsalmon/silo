# Silo

Silo is a design for running disposable rootless OCI jobs and private TCP services on a restricted Linux worker. An operator sets which jobs an agent can launch, which named volumes jobs can mount, and which service endpoints they can reach.

- [Runtime design](docs/design.md)
- [CLI design](docs/cli.md)

This branch contains design documents, not an implementation. Local prototypes demonstrated named TCP access between private namespaces and connected socket descriptor passing. The integrated runner, persistent Postgres workload, and production resource limits remain to be validated. Silo shares the worker kernel; it is not a VM-grade isolation boundary.
