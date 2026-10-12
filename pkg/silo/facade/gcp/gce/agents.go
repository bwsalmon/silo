package gce

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"

	"github.com/bwsalmon/silo/pkg/silo/agents"
	gceapi "github.com/bwsalmon/silo/pkg/silo/backend/gcp/gce"
	"github.com/bwsalmon/silo/pkg/silo/fenceapi"
	"github.com/bwsalmon/silo/pkg/silo/ids"
	"github.com/bwsalmon/silo/pkg/silo/store"
	"github.com/bwsalmon/silo/pkg/silo/tree"
)

// Agents' tool VMs (agents.md, "Creation", SV-104). silod makes an
// agent's tool VM with an ordinary instances.insert on this facade,
// served in-process, so every check, charge and rewrite applies. The
// call is marked as the agent's: only such a call sets the hidden
// label silo-agent=<agent id>, and only such a call may change an
// instance carrying it. Any other write to a tool VM is a 403 naming
// the agent (call 40).

type agentCallKey struct{}

// withAgent marks ctx as silod's own call for agent's tool VM.
func withAgent(ctx context.Context, agent string) context.Context {
	return context.WithValue(ctx, agentCallKey{}, agent)
}

// agentCall is the agent a call is silod's own for, or "".
func agentCall(ctx context.Context) string {
	a, _ := ctx.Value(agentCallKey{}).(string)
	return a
}

// ToolVMs is the fenceapi.ToolVMs that makes tool VMs through s.
func (s *Server) ToolVMs() fenceapi.ToolVMs { return toolVMs{s} }

type toolVMs struct{ s *Server }

// toolVMName is the name of a tool VM an agent record names: an agent
// made before tenants used real names (gcp-backend.md, "Naming",
// SV-76) recorded agent-<name>, without the silo's prefix its VM
// carries.
func toolVMName(silo *tree.Silo, instance string) string {
	if p := ids.NamePrefix(silo.ID); !strings.HasPrefix(instance, p) {
		return p + instance
	}
	return instance
}

// Create implements fenceapi.ToolVMs.
func (v toolVMs) Create(ctx context.Context, caller fenceapi.Caller, silo *tree.Silo, agent, instance string, spec fenceapi.ToolVMSpec, script string) (string, error) {
	items := []map[string]string{{"key": agents.StartupScriptKey, "value": script}}
	var keys []string
	for k := range spec.Metadata {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		items = append(items, map[string]string{"key": k, "value": spec.Metadata[k]})
	}
	region := gceapi.ZoneRegion(spec.Zone)
	body := map[string]any{
		"name":              instance,
		"description":       "the tool VM of a Silo agent",
		"machineType":       "zones/" + spec.Zone + "/machineTypes/" + spec.MachineType,
		"networkInterfaces": []any{map[string]any{"subnetwork": "regions/" + region + "/subnetworks/" + spec.Subnet}},
		"disks": []any{map[string]any{"boot": true, "autoDelete": true, "initializeParams": map[string]any{
			"sourceImage": spec.Image, "diskSizeGb": strconv.FormatInt(spec.DiskGB, 10)}}},
		"metadata": map[string]any{"items": items},
	}
	if spec.Spot {
		body["scheduling"] = map[string]any{"provisioningModel": "SPOT"}
	}
	base := "projects/silo-" + silo.ID + "/zones/" + spec.Zone + "/instances"
	if _, err := v.call(ctx, caller, agent, http.MethodPost, base, body); err != nil {
		return "", err
	}
	out, err := v.call(ctx, caller, agent, http.MethodGet, base+"/"+instance, nil)
	if err != nil {
		return "", nil // made; its address shows once GCE has it
	}
	var inst struct {
		NetworkInterfaces []struct {
			NetworkIP string `json:"networkIP"`
		} `json:"networkInterfaces"`
	}
	_ = json.Unmarshal(out, &inst)
	if len(inst.NetworkInterfaces) > 0 {
		return inst.NetworkInterfaces[0].NetworkIP, nil
	}
	return "", nil
}

// Identify implements fenceapi.ToolVMIdentifier: the instance's GCE
// id and its address now, read through the facade as caller like any
// other call of silod's for the agent.
func (v toolVMs) Identify(ctx context.Context, caller fenceapi.Caller, silo *tree.Silo, agent, zone, instance string) (string, string, error) {
	instance = toolVMName(silo, instance)
	out, err := v.call(ctx, caller, agent, http.MethodGet, "projects/silo-"+silo.ID+"/zones/"+zone+"/instances/"+instance, nil)
	if err != nil {
		return "", "", err
	}
	var inst struct {
		ID                json.Number `json:"id"`
		NetworkInterfaces []struct {
			NetworkIP string `json:"networkIP"`
		} `json:"networkInterfaces"`
	}
	if err := json.Unmarshal(out, &inst); err != nil {
		return "", "", err
	}
	ip := ""
	if len(inst.NetworkInterfaces) > 0 {
		ip = inst.NetworkInterfaces[0].NetworkIP
	}
	return inst.ID.String(), ip, nil
}

// Delete implements fenceapi.ToolVMs.
func (v toolVMs) Delete(ctx context.Context, caller fenceapi.Caller, silo *tree.Silo, agent, zone, instance string) error {
	instance = toolVMName(silo, instance)
	_, err := v.call(ctx, caller, agent, http.MethodDelete, "projects/silo-"+silo.ID+"/zones/"+zone+"/instances/"+instance, nil)
	var e *fenceapi.Error
	if err != nil && asFence(err, &e) && e.Code == fenceapi.NotFound {
		return nil
	}
	return err
}

func asFence(err error, e **fenceapi.Error) bool {
	x, ok := err.(*fenceapi.Error)
	*e = x
	return ok
}

// call serves one facade call in-process, as caller, marked as agent's,
// and returns its body, or its refusal as a fence API error: 403
// quotaExceeded as QUOTA_EXCEEDED, any other 403 as PERMISSION_DENIED,
// 400 and 404 (a subnet or image that isn't there) as
// INVALID_ARGUMENT, and the rest as UNAVAILABLE.
func (v toolVMs) call(ctx context.Context, caller fenceapi.Caller, agent, method, rest string, body any) ([]byte, error) {
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	ctx = fenceapi.ContextWithCaller(withAgent(ctx, agent), caller)
	r, err := http.NewRequestWithContext(ctx, method, "/compute/v1/"+rest, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	w := httptest.NewRecorder()
	v.s.ServeHTTP(w, r)
	if w.Code == http.StatusOK {
		return w.Body.Bytes(), nil
	}
	var env struct {
		Error struct {
			Message string `json:"message"`
			Errors  []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	reason := ""
	if len(env.Error.Errors) > 0 {
		reason = env.Error.Errors[0].Reason
	}
	e := &fenceapi.Error{Message: "tool VM: " + env.Error.Message,
		Details: map[string]any{"facade_status": w.Code, "facade_reason": reason}}
	switch {
	case w.Code == http.StatusForbidden && reason == ReasonQuotaExceeded:
		e.Code = fenceapi.QuotaExceeded
	case w.Code == http.StatusForbidden:
		e.Code = fenceapi.PermissionDenied
	case w.Code == http.StatusNotFound && method == http.MethodDelete:
		e.Code = fenceapi.NotFound
	case w.Code == http.StatusBadRequest, w.Code == http.StatusNotFound:
		e.Code = fenceapi.InvalidArgument
	case w.Code == http.StatusConflict:
		e.Code = fenceapi.AlreadyExists
	default:
		e.Code = fenceapi.Unavailable
	}
	return nil, e
}

// instanceReads are the instance methods that only read, or make a new
// instance, so need no tool-VM check.
var instanceReads = map[string]bool{"insert": true, "get": true, "list": true, "aggregatedList": true, "getSerialPortOutput": true}

// refuseToolVM refuses a write to an agent's tool VM that isn't silod's
// own call for that agent: a 403 naming the agent (call 40).
func (s *Server) refuseToolVM(q *request) error {
	c := q.call
	if c.Resource != "instances" || instanceReads[c.Method] || c.Name == "" || q.n.checkName(c.Name) != nil {
		return nil
	}
	inst, err := s.gce.GetInstance(q.ctx(), s.opts.HostProject, c.Zone, c.Name)
	if err != nil || !q.owns(inst.Name, inst.Labels) {
		return nil // the handler says 404, or whatever GCE says
	}
	agent := inst.Labels[agents.LabelAgent]
	if agent == "" || agent == agentCall(q.ctx()) {
		return nil
	}
	path := "-" + agent
	_ = s.st.Tx(q.ctx(), func(tx store.Tx) error {
		path, err = fenceapi.AgentPath(q.ctx(), tx, agent)
		return err
	})
	return forbidden("silo: instance %s is the tool VM of agent %s; it is managed through the agent (stop, resume or delete the agent)", c.Name, path)
}
