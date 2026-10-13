#!/usr/bin/env python3
# Writes the spike's two OCI bundles from `runc spec`'s default:
#   runc/  -- joins the pinned user namespace and netns by path;
#   runsc/ -- names only the netns; runsc is started inside the user
#             namespace instead, because its gofer cannot setns into a
#             user namespace (a multi-threaded process gets EINVAL).
import json, os, sys

b = sys.argv[1]
spec = json.load(open(os.path.join(b, "config.json")))
spec["process"]["terminal"] = False
spec["process"]["args"] = ["/bin/sh", "-c", "id; cat /proc/self/uid_map; ip addr show eth0"]
spec["root"] = {"path": os.path.join(b, "rootfs"), "readonly": False}
spec["linux"].pop("resources", None)
base = [n for n in spec["linux"]["namespaces"] if n["type"] not in ("network", "user")]

for name, ns in (
    ("runc", base + [{"type": "user", "path": "/run/silo/netns/c1.user"},
                     {"type": "network", "path": "/run/silo/netns/c1.net"}]),
    ("runsc", base + [{"type": "network", "path": "/run/silo/netns/c1.net"}]),
):
    os.makedirs(os.path.join(b, name), exist_ok=True)
    s = dict(spec, linux=dict(spec["linux"], namespaces=ns))
    if name == "runc":
        # runc before 1.2 wants the joined namespace's mapping repeated.
        m = [{"containerID": 0, "hostID": 1, "size": 65536}]
        s["linux"]["uidMappings"] = s["linux"]["gidMappings"] = m
    json.dump(s, open(os.path.join(b, name, "config.json"), "w"), indent=1)
