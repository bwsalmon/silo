package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"

	"github.com/bwsalmon/silo/pkg/backend/local/runtime"
	"github.com/bwsalmon/silo/pkg/fenceapi"
	"github.com/bwsalmon/silo/pkg/store"
	"github.com/bwsalmon/silo/pkg/tree"
)

// Agents' tool containers (local-backend.md §10): on the local backend
// an agent's tools run in a container of its silo, in place of the GCP
// backend's tool VM. silod makes it with an ordinary container create
// and start on this facade, served in-process, so every check, charge
// and audit applies; the call is marked as the agent's, and only such a
// call makes a container its tool container, or may change one. Any
// other write to it is a 403 naming the agent, as call 40 has it for a
// tool VM.
//
// The container runs silo-tools, which dials the gateway as it does on
// a tool VM. In the sandbox class silo-tools is silod's own static
// binary, which the driver mounts read-only at runtime.ToolsPath; the
// container's image is any the silo has (Options.ToolImage by
// default). In the vm class the image must carry silo-tools at
// runtime.ToolsPath itself: the tool image install builds
// (Options.VMToolImage), or an agent's own.

type agentCallKey struct{}

// withAgent marks ctx as silod's own call for agent's tool container.
func withAgent(ctx context.Context, agent string) context.Context {
	return context.WithValue(ctx, agentCallKey{}, agent)
}

// agentCall is the agent a call is silod's own for, or "".
func agentCall(ctx context.Context) string {
	a, _ := ctx.Value(agentCallKey{}).(string)
	return a
}

// refuseToolContainer refuses a write to an agent's tool container
// that isn't silod's own call for that agent.
func (s *Server) refuseToolContainer(q *request, c *Container) error {
	if c == nil || c.Agent == "" || c.Agent == agentCall(q.ctx()) {
		return nil
	}
	path := "-" + c.Agent
	_ = s.st.Tx(q.ctx(), func(tx store.Tx) error {
		p, err := fenceapi.AgentPath(q.ctx(), tx, c.Agent)
		if err == nil {
			path = p
		}
		return nil
	})
	return forbidden("silo: container %s is the tool container of agent %s; it is managed through the agent (stop, resume or delete the agent)", c.Name, path)
}

// ToolContainers is the fenceapi.ToolVMs that makes agents' tool
// containers through s.
func (s *Server) ToolContainers() *ToolContainers { return &ToolContainers{s: s} }

// ToolContainers makes, identifies and deletes agents' tool containers.
type ToolContainers struct{ s *Server }

var (
	_ fenceapi.ToolVMs          = (*ToolContainers)(nil)
	_ fenceapi.ToolVMIdentifier = (*ToolContainers)(nil)
	_ fenceapi.ToolSpecChecker  = (*ToolContainers)(nil)
)

// ToolZone is the zone a local tool container's record carries: the
// host is one.
const ToolZone = "local"

// ToolArgs are silo-tools' arguments in a tool container: commands run
// as the container's own root (a subuid on the host, fenced), and the
// silo's names carry no prefix, so there is no metadata server to ask.
var ToolArgs = []string{"--user=", "--name-prefix=-"}

// CheckToolSpec implements fenceapi.ToolSpecChecker: a tool container
// has an image and a class, and none of a tool VM's GCE fields.
func (t *ToolContainers) CheckToolSpec(vm *fenceapi.ToolVMSpec) error {
	field := ""
	switch {
	case vm.Zone != "" && vm.Zone != ToolZone:
		field = "tool_vm.zone"
	case vm.Subnet != "":
		field = "tool_vm.subnet"
	case vm.MachineType != "":
		field = "tool_vm.machine_type"
	case vm.DiskGB != 0:
		field = "tool_vm.disk_gb"
	case vm.Spot:
		field = "tool_vm.spot"
	case len(vm.Metadata) > 0:
		field = "tool_vm.metadata"
	}
	if field != "" {
		return &fenceapi.Error{Code: fenceapi.InvalidArgument, Message: field + " is a GCE tool VM's; on the local backend an agent's tools run in a container (tool_vm.image and tool_vm.isolation)",
			Details: map[string]any{"field": field}}
	}
	switch runtime.Class(vm.Isolation) {
	case "", runtime.VM, runtime.Sandbox:
	default:
		return &fenceapi.Error{Code: fenceapi.InvalidArgument, Message: "tool_vm.isolation " + vm.Isolation + ": want vm or sandbox",
			Details: map[string]any{"field": "tool_vm.isolation"}}
	}
	vm.Zone = ToolZone
	return nil
}

// image is the tool container's image in class.
func (t *ToolContainers) image(vm fenceapi.ToolVMSpec, class runtime.Class) (string, error) {
	if vm.Image != "" {
		return vm.Image, nil
	}
	img := t.s.opts.ToolImage
	if class == runtime.VM {
		img = t.s.opts.VMToolImage
	}
	if img == "" {
		what := "an image the silo has, any with a shell: silo-tools is mounted in"
		if class == runtime.VM {
			what = "an image carrying silo-tools at " + runtime.ToolsPath + ", as install's vm tool image will"
		}
		return "", &fenceapi.Error{Code: fenceapi.InvalidArgument, Message: "tool_vm.image is required on this host: " + what,
			Details: map[string]any{"field": "tool_vm.image"}}
	}
	return img, nil
}

// Create implements fenceapi.ToolVMs: it creates and starts the
// container instance in silo, as caller, marked as agent's, and
// returns its address.
func (t *ToolContainers) Create(ctx context.Context, caller fenceapi.Caller, silo *tree.Silo, agent, instance string, vm fenceapi.ToolVMSpec, _ string) (string, error) {
	class := runtime.Class(vm.Isolation)
	if class == "" {
		var iso fenceapi.Isolation
		if err := t.s.st.Tx(ctx, func(tx store.Tx) error {
			chain, err := tree.Ancestry(ctx, tx, silo.ID)
			if err != nil {
				return err
			}
			iso, _, err = fenceapi.EffectiveIsolation(ctx, tx, chain)
			return err
		}); err != nil {
			return "", err
		}
		class = runtime.Class(iso.Default)
	}
	img, err := t.image(vm, class)
	if err != nil {
		return "", err
	}
	body := map[string]any{
		"Image":      img,
		"Entrypoint": []string{runtime.ToolsPath},
		"Cmd":        ToolArgs,
		"HostConfig": map[string]any{"Runtime": string(class)},
	}
	if _, err := t.call(ctx, caller, silo, agent, http.MethodPost, "/containers/create?name="+url.QueryEscape(instance), body); err != nil {
		return "", err
	}
	if _, err := t.call(ctx, caller, silo, agent, http.MethodPost, "/containers/"+url.PathEscape(instance)+"/start", nil); err != nil {
		t.call(context.WithoutCancel(ctx), caller, silo, agent, http.MethodDelete, "/containers/"+url.PathEscape(instance)+"?force=1", nil)
		return "", err
	}
	_, ip, err := t.Identify(ctx, caller, silo, agent, ToolZone, instance)
	if err != nil {
		return "", nil // made; its address shows on the next identify
	}
	return ip, nil
}

// Identify implements fenceapi.ToolVMIdentifier: the container's id and
// its address on its first network, read through the facade as caller.
func (t *ToolContainers) Identify(ctx context.Context, caller fenceapi.Caller, silo *tree.Silo, agent, _, instance string) (string, string, error) {
	b, err := t.call(ctx, caller, silo, agent, http.MethodGet, "/containers/"+url.PathEscape(instance)+"/json", nil)
	if err != nil {
		return "", "", err
	}
	var c struct {
		ID              string `json:"Id"`
		NetworkSettings struct {
			Networks map[string]struct {
				IPAddress string
			}
		}
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return "", "", err
	}
	var names []string
	for n := range c.NetworkSettings.Networks {
		names = append(names, n)
	}
	sort.Strings(names)
	ip := ""
	for _, n := range names {
		if a := c.NetworkSettings.Networks[n].IPAddress; a != "" {
			ip = a
			break
		}
	}
	return c.ID, ip, nil
}

// Delete implements fenceapi.ToolVMs; one that is gone is not an error.
func (t *ToolContainers) Delete(ctx context.Context, caller fenceapi.Caller, silo *tree.Silo, agent, _, instance string) error {
	_, err := t.call(ctx, caller, silo, agent, http.MethodDelete, "/containers/"+url.PathEscape(instance)+"?force=1", nil)
	if e, ok := err.(*fenceapi.Error); ok && e.Code == fenceapi.NotFound {
		return nil
	}
	return err
}

// call serves one facade call in-process on silo, as caller, marked as
// agent's, and returns its body, or its refusal as a fence API error.
func (t *ToolContainers) call(ctx context.Context, caller fenceapi.Caller, silo *tree.Silo, agent, method, path string, body any) ([]byte, error) {
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	ctx = ContextWithSocket(fenceapi.ContextWithCaller(withAgent(ctx, agent), caller), silo.ID)
	r, err := http.NewRequestWithContext(ctx, method, "/v"+APIVersion+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	t.s.ServeHTTP(w, r)
	if w.Code < 300 {
		return w.Body.Bytes(), nil
	}
	var env struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	e := &fenceapi.Error{Message: "tool container: " + env.Message, Details: map[string]any{"facade_status": w.Code}}
	switch w.Code {
	case http.StatusForbidden:
		e.Code = fenceapi.PermissionDenied
	case http.StatusNotFound:
		e.Code = fenceapi.NotFound
		if method == http.MethodPost {
			e.Code = fenceapi.InvalidArgument // an image the silo doesn't have
		}
	case http.StatusBadRequest:
		e.Code = fenceapi.InvalidArgument
	case http.StatusConflict:
		e.Code = fenceapi.AlreadyExists
		if strings.HasPrefix(env.Message, "silo: quota exceeded") {
			e.Code = fenceapi.QuotaExceeded
		}
	default:
		e.Code = fenceapi.Unavailable
	}
	return nil, e
}
