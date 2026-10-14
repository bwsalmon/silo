package k8s

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// Labels and annotations on everything a session makes (design §5.4):
// silo gc finds them by label, and only removes those whose origin is
// this machine and user. The origin is an annotation because a label
// value cannot hold its colon.
const (
	ManagedByLabel   = "app.kubernetes.io/managed-by"
	ManagedBy        = "silo"
	SessionLabel     = "silo.dev/session"
	CheckLabel       = "silo.dev/check"
	OriginAnnotation = "silo.dev/origin"
)

// Paths and ports inside the pod.
const (
	// PodDir is the session directory in the agent container, an
	// emptyDir: staged files under agent/, the default workdir work/.
	PodDir = "/silo"
	// SecretDir is where the session's Secret is mounted: one file per
	// credential-carrying variable of the CLI's environment.
	SecretDir = "/run/silo/secret"
	// GatewayPort and ControlPort are the tunnel sidecar's listeners on
	// the pod's loopback: the CLI's gateway URL, and silo's
	// port-forward.
	GatewayPort = 7811
	ControlPort = 7812
)

// Container names.
const (
	AgentContainer  = "agent"
	TunnelContainer = "tunnel"
)

// DefaultTunnelImage is the published tunnel sidecar (README, design §5.4),
// built from cmd/silo-tunnel/Dockerfile.
const DefaultTunnelImage = "ghcr.io/bwsalmon/silo-tunnel:v2"

// holdScript keeps the agent container running until silo execs the CLI
// into it, and stops at once when the pod is deleted.
const holdScript = `trap 'exit 0' TERM INT; while :; do sleep 3600 & wait $!; done`

// PodName is the session's pod, and its Secret.
func PodName(id string) string { return "silo-" + id }

// Resources is the agent container's requests and limits: cpu a
// Kubernetes quantity, memory in bytes, either zero for none.
type Resources struct {
	CPU    string
	Memory int64
}

type obj = map[string]any

func (r Resources) spec() obj {
	q := obj{}
	if r.CPU != "" {
		q["cpu"] = r.CPU
	}
	if r.Memory > 0 {
		q["memory"] = strconv.FormatInt(r.Memory, 10)
	}
	if len(q) == 0 {
		return obj{}
	}
	return obj{"requests": q, "limits": q}
}

func meta(name, id, origin string) obj {
	labels := obj{ManagedByLabel: ManagedBy}
	if id != "" {
		labels[SessionLabel] = id
	}
	return obj{"name": name, "labels": labels, "annotations": obj{OriginAnnotation: origin}}
}

// podSpec is the session's pod: the agent container holding until silo
// execs the CLI, and the tunnel sidecar. Neither gets a service account
// token, so the agent has no credentials for the cluster; the Secret is
// mounted only into the agent container.
func podSpec(name, id, origin, image, tunnelImage string, res Resources, secret bool) []byte {
	mounts := []obj{{"name": "silo", "mountPath": PodDir}}
	volumes := []obj{{"name": "silo", "emptyDir": obj{}}}
	if secret {
		mounts = append(mounts, obj{"name": "secret", "mountPath": SecretDir, "readOnly": true})
		// 0444: the agent container's user is the image's, which silo
		// does not know, and the file's only reader is that container.
		volumes = append(volumes, obj{"name": "secret", "secret": obj{"secretName": name, "defaultMode": 0o444}})
	}
	pod := obj{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   meta(name, id, origin),
		"spec": obj{
			"restartPolicy":                 "Never",
			"terminationGracePeriodSeconds": 5,
			"automountServiceAccountToken":  false,
			"enableServiceLinks":            false,
			"containers": []obj{
				{
					"name":         AgentContainer,
					"image":        image,
					"command":      []string{"/bin/sh", "-c", holdScript},
					"resources":    res.spec(),
					"volumeMounts": mounts,
				},
				{
					"name":  TunnelContainer,
					"image": tunnelImage,
					"args": []string{
						"-gateway", fmt.Sprintf("127.0.0.1:%d", GatewayPort),
						"-control", fmt.Sprintf("127.0.0.1:%d", ControlPort),
					},
					"resources": obj{
						"requests": obj{"cpu": "10m", "memory": "16Mi"},
						"limits":   obj{"memory": "64Mi"},
					},
					"securityContext": obj{
						"runAsNonRoot":             true,
						"runAsUser":                65532,
						"allowPrivilegeEscalation": false,
						"readOnlyRootFilesystem":   true,
						"capabilities":             obj{"drop": []string{"ALL"}},
					},
				},
			},
			"volumes": volumes,
		},
	}
	b, _ := json.Marshal(pod)
	return b
}

// secretSpec is the session's Secret, owned by its pod so that the
// cluster deletes it with the pod even when silo cannot.
func secretSpec(name, id, origin, podUID string, data map[string]string) []byte {
	m := meta(name, id, origin)
	if podUID != "" {
		m["ownerReferences"] = []obj{{"apiVersion": "v1", "kind": "Pod", "name": name, "uid": podUID}}
	}
	b, _ := json.Marshal(obj{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   m,
		"type":       "Opaque",
		"stringData": data,
	})
	return b
}

// checkPodSpec is --dry-run's short-lived pod: the agent image runs the
// probe script and the tunnel image its -check, both once, so that
// pulling either is tried on the cluster's own nodes with its own
// credentials.
func checkPodSpec(name, origin, image, tunnelImage, probe string) []byte {
	m := meta(name, "", origin)
	m["labels"].(obj)[CheckLabel] = "true"
	b, _ := json.Marshal(obj{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   m,
		"spec": obj{
			"restartPolicy":                 "Never",
			"terminationGracePeriodSeconds": 0,
			"automountServiceAccountToken":  false,
			"enableServiceLinks":            false,
			"activeDeadlineSeconds":         300,
			"containers": []obj{
				{"name": AgentContainer, "image": image, "command": []string{"/bin/sh", "-c", probe}},
				{"name": TunnelContainer, "image": tunnelImage, "args": []string{"-check"}},
			},
		},
	})
	return b
}

// podStatus is the part of a pod's status silo reads.
type podStatus struct {
	Metadata struct {
		UID               string `json:"uid"`
		DeletionTimestamp string `json:"deletionTimestamp"`
	} `json:"metadata"`
	Status struct {
		Phase      string `json:"phase"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
		ContainerStatuses []containerStatus `json:"containerStatuses"`
	} `json:"status"`
}

type containerStatus struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	State struct {
		Waiting *struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"waiting"`
		Terminated *struct {
			ExitCode int    `json:"exitCode"`
			Reason   string `json:"reason"`
		} `json:"terminated"`
	} `json:"state"`
}

func (p *podStatus) ready() bool {
	for _, c := range p.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}

// pullFailure names a container whose image cannot be pulled or whose
// container cannot be made, with the kubelet's reason; empty when none.
func (p *podStatus) failure() (container, reason, message string) {
	for _, c := range p.Status.ContainerStatuses {
		if w := c.State.Waiting; w != nil {
			switch w.Reason {
			case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "ErrImageNeverPull", "CreateContainerConfigError", "CreateContainerError", "RunContainerError":
				return c.Name, w.Reason, w.Message
			}
		}
	}
	return "", "", ""
}
