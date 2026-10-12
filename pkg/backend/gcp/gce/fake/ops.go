package fake

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/gcp/gce"
	compute "google.golang.org/api/compute/v1"
)

// FailureKind is how FailOperation makes an operation fail.
type FailureKind int

const (
	// FailQuota is a quota refusal GCE reports in the operation
	// (QUOTA_EXCEEDED), such as a per-family quota silod doesn't
	// predict.
	FailQuota FailureKind = iota + 1
	// FailStockout is a zone out of capacity
	// (ZONE_RESOURCE_POOL_EXHAUSTED).
	FailStockout
	// FailInternal is GCE's INTERNAL_ERROR.
	FailInternal
)

// Failure selects the operation FailOperation fails.
type Failure struct {
	// Method is the API method, as "instances.insert", "disks.resize"
	// or "networkFirewallPolicies.addRule". Empty matches any.
	Method string
	// Name is the target resource's name. Empty matches any.
	Name string
	Kind FailureKind
}

// FailOperation makes the next operation that finishes and matches fl
// fail with fl.Kind, leaving GCE as it was before the call. Failures
// queue: each matches one operation.
func (f *Fake) FailOperation(fl Failure) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = append(f.failures, fl)
}

// opErr is a failed operation's error.
type opErr struct {
	code   string
	status int64
	msg    string
}

func (k FailureKind) err(o *op) *opErr {
	switch k {
	case FailQuota:
		return &opErr{gce.CodeQuotaExceeded, http.StatusForbidden, "Quota 'N2_CPUS' exceeded.  Limit: 0.0 in region " + o.region() + "."}
	case FailStockout:
		return &opErr{gce.CodeStockout, http.StatusServiceUnavailable,
			"The zone '" + trimBase(o.op.Zone) + "' does not have enough resources available to fulfill the request.  Try a different zone, or try again later."}
	}
	return &opErr{gce.CodeInternal, http.StatusInternalServerError, "Internal error. Please try again or contact Google Support."}
}

// op is one operation and what finishing it does.
type op struct {
	op     *compute.Operation
	method string
	target string
	// commit applies the change, under the lock. A non-nil result fails
	// the operation, and abort is then called.
	commit func() *opErr
	// abort undoes what the call did before the operation finished (an
	// insert's placeholder). May be nil.
	abort func()
	done  chan struct{}
}

func (o *op) region() string {
	if o.op.Region != "" {
		return gce.LastSegment(o.op.Region)
	}
	return gce.ZoneRegion(gce.LastSegment(o.op.Zone))
}

// start begins an operation on targetLink, under the lock, and returns
// a copy of it as GCE returns it from the call.
func (f *Fake) start(p *state, method, targetLink string, targetID uint64, commit func() *opErr, abort func()) *compute.Operation {
	ref := gce.ParseRef(targetLink)
	scope := gce.Scope{Zone: ref.Zone, Region: ref.Region}
	f.seq++
	name := fmt.Sprintf("operation-%d-%013x", f.seq, f.seq*7919)
	_, opType, _ := strings.Cut(method, ".")
	o := &op{
		method: method,
		target: ref.Name,
		commit: commit,
		abort:  abort,
		done:   make(chan struct{}),
		op: &compute.Operation{
			Kind:          "compute#operation",
			Id:            f.nextID(),
			Name:          name,
			OperationType: opType,
			TargetLink:    targetLink,
			TargetId:      targetID,
			Status:        "RUNNING",
			Progress:      0,
			InsertTime:    f.timestamp(),
			StartTime:     f.timestamp(),
			User:          "fake@" + p.name + ".iam.gserviceaccount.com",
			SelfLink:      p.link(scope.String(), "operations", name),
		},
	}
	if scope.Zone != "" {
		o.op.Zone = gce.SelfLinkBase + "projects/" + p.name + "/zones/" + scope.Zone
	}
	if scope.Region != "" {
		o.op.Region = gce.SelfLinkBase + "projects/" + p.name + "/regions/" + scope.Region
	}
	p.operations.put(scope.String(), name, o)
	f.opOrder = append(f.opOrder, o)
	if !f.opts.ManualOperations {
		go func() {
			if f.opts.OperationLatency > 0 {
				time.Sleep(f.opts.OperationLatency)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			f.finish(o)
		}()
	}
	return clone(o.op)
}

// finish ends o, under the lock.
func (f *Fake) finish(o *op) {
	if o.op.Status == "DONE" {
		return
	}
	var e *opErr
	for i, fl := range f.failures {
		if (fl.Method == "" || fl.Method == o.method) && (fl.Name == "" || fl.Name == o.target) {
			f.failures = append(f.failures[:i:i], f.failures[i+1:]...)
			e = fl.Kind.err(o)
			break
		}
	}
	if e == nil {
		e = o.commit()
	}
	if e != nil {
		if o.abort != nil {
			o.abort()
		}
		o.op.Error = &compute.OperationError{Errors: []*compute.OperationErrorErrors{{Code: e.code, Message: e.msg}}}
		o.op.HttpErrorStatusCode = e.status
		o.op.HttpErrorMessage = http.StatusText(int(e.status))
	}
	o.op.Status = "DONE"
	o.op.Progress = 100
	o.op.EndTime = f.timestamp()
	for i, x := range f.opOrder {
		if x == o {
			f.opOrder = append(f.opOrder[:i:i], f.opOrder[i+1:]...)
			break
		}
	}
	close(o.done)
}

// RunOperations finishes every operation still running, in the order
// they started, and returns how many it finished.
func (f *Fake) RunOperations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for len(f.opOrder) > 0 {
		f.finish(f.opOrder[0])
		n++
	}
	return n
}

func (f *Fake) findOp(ctx context.Context, project string, scope gce.Scope, name string) (*op, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	if err := f.checkScope(scope); err != nil {
		return nil, err
	}
	o := p.operations.get(scope.String(), name)
	if o == nil {
		return nil, notFound(p.link(scope.String(), "operations", name))
	}
	return o, nil
}

func (f *Fake) checkScope(scope gce.Scope) error {
	if scope.Zone != "" {
		return f.zoneExists(scope.Zone)
	}
	if scope.Region != "" {
		return f.regionExists(scope.Region)
	}
	return nil
}

// GetOperation implements gce.Client.
func (f *Fake) GetOperation(ctx context.Context, project string, scope gce.Scope, name string) (*compute.Operation, error) {
	o, err := f.findOp(ctx, project, scope, name)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return clone(o.op), nil
}

// WaitOperation implements gce.Client.
func (f *Fake) WaitOperation(ctx context.Context, project string, scope gce.Scope, name string) (*compute.Operation, error) {
	o, err := f.findOp(ctx, project, scope, name)
	if err != nil {
		return nil, err
	}
	t := time.NewTimer(f.opts.WaitTimeout)
	defer t.Stop()
	select {
	case <-o.done:
	case <-t.C:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return clone(o.op), nil
}

// ListOperations implements gce.Client.
func (f *Fake) ListOperations(ctx context.Context, project string, scope gce.Scope) ([]*compute.Operation, error) {
	p, err := f.lock(ctx, project)
	if err != nil {
		return nil, err
	}
	defer f.unlock()
	if err := f.checkScope(scope); err != nil {
		return nil, err
	}
	var out []*compute.Operation
	for _, o := range p.operations.list(scope.String()) {
		out = append(out, clone(o.op))
	}
	return out, nil
}
