// Package gvisor is the sandbox class's runtime driver
// (docs/silo/local-backend.md §6, "gVisor driver"; §15, step 7): a
// runtime.Driver on silo-containerd, the containerd silod runs inside
// silo-nsd, with runsc through containerd's runsc shim on the systrap
// platform and netstack.
//
//   - Containers are containerd containers in rootless.ContainerdNamespace,
//     labelled dev.silo.class=sandbox, and their tasks run on the shim's
//     runtime. Create makes the task (runsc create) as well, so that a
//     created container already has its sandbox, on the netns silo-nsd
//     made and wired before Create was called.
//   - runsc is started by `silod runsc` inside the container's user
//     namespace (rootless.SandboxPatch, call L30); the container's
//     snapshot is remapped to that namespace's root by a helper run inside
//     the holder (RemapMain), since remapping mounts the snapshot and
//     silod, on the host, can't.
//   - The workload's stdout and stderr go to FIFOs the shim writes, read
//     by a log copier (LogMain) that outlives silod and writes Docker's
//     json-file format. Exec uses FIFOs of its own.
//   - Archive (docker cp) runs tar in the container through exec, from a
//     static busybox bind-mounted read-only at ToolboxPath when the
//     driver is given one: with --overlay2=root:self the container's
//     writes live in the sandbox, not on the host.
//   - RootfsUpper (docker commit) is `runsc tar rootfs-upper`, run by
//     `silod runsc` in the holder as the shim runs runsc, its tar
//     streamed through a pipe of its own.
//   - Stats are the shim's cgroup metrics; a container's cgroup is its
//     scope in its silo's slice when cgroups are delegated
//     (rootless.CgroupsPath).
//   - Adopt finds the containers a previous silod left running and
//     restarts any log copier that died; containerd's shims outlive both
//     silod and containerd (§12, "Restart and adoption").
package gvisor
