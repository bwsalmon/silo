// Package granule is the vm class's runtime pieces
// (docs/silo/local-backend.md §6, "granule driver"; §15, step 10): a
// granule microVM per container, run as one granule VM container on
// silo-containerd under crun, rootless (call L31).
//
//   - VM and SpecOpts are the VM container: the granule image's
//     granule-shim in run mode, with the settings `granule vm create`
//     derives from its flags, the container's process as granule's
//     per-VM override, its streams, exit status and stdin through
//     granule's workload I/O, and its volumes as virtio-fs shares.
//     TestVMCreateMatchesGranule checks it against what `granule vm
//     create` passes to docker run at the pinned Commit, recorded
//     through granule's fake docker (testdata/record.sh).
//   - ProbeHost is the class's host check: /dev/kvm creates a VM and
//     /dev/net/tun opens. Nothing falls back to TCG.
//   - Virtiofsd is the per-volume virtiofsd that serves a share.
//   - WrapArgs is `granule wrap` without docker, which turns an image
//     into the guest half a VM container boots.
package granule
