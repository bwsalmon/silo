package install

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Services are the APIs the apis step enables (install.md step 2).
// cloudbilling is read only.
var Services = []string{
	"compute.googleapis.com",
	"dns.googleapis.com",
	"iam.googleapis.com",
	"iap.googleapis.com",
	"artifactregistry.googleapis.com",
	"cloudresourcemanager.googleapis.com",
	"serviceusage.googleapis.com",
	"logging.googleapis.com",
	"sqladmin.googleapis.com",
	"cloudquotas.googleapis.com",
	"cloudbilling.googleapis.com",
}

// APIs is step 2, apis: it enables Services. Uninstall leaves them on
// (open question 11) and says which.
type APIs struct{ Env *Env }

// APIsObserved is which of Services the project has enabled.
type APIsObserved struct {
	Enabled []string `json:"enabled"`
}

func (s *APIs) Name() string          { return "apis" }
func (s *APIs) NewObserved() Observed { return &APIsObserved{} }

func (s *APIs) Observe(ctx context.Context) (Observed, error) {
	obs := &APIsObserved{Enabled: []string{}}
	for _, svc := range Services {
		on, err := s.Env.Enabled(ctx, svc)
		if err != nil {
			return nil, err
		}
		if on {
			obs.Enabled = append(obs.Enabled, svc)
		}
	}
	return obs, nil
}

func (s *APIs) Plan(o Observed) []Change {
	on := enabledSet(o)
	var out []Change
	for _, svc := range Services {
		if !on[svc] {
			out = append(out, Change{Action: ActionEnable, Object: svc, Estimate: 15 * time.Second})
		}
	}
	if len(out) == 0 {
		return []Change{{Action: ActionOK, Detail: fmt.Sprintf("%d APIs enabled", len(Services))}}
	}
	return out
}

func (s *APIs) PlanRemove(o Observed) []Change {
	on := enabledSet(o)
	var names []string
	for svc := range on {
		names = append(names, strings.TrimSuffix(svc, ".googleapis.com"))
	}
	sort.Strings(names)
	if len(names) == 0 {
		return []Change{{Action: ActionOK, Detail: "none of install's APIs is enabled"}}
	}
	return []Change{{Action: ActionKeep, Detail: "leaves on " + strings.Join(names, ", "),
		Why: "uninstall doesn't disable APIs: something else in the project may use them"}}
}

func (s *APIs) Apply(ctx context.Context, changes []Change) error {
	var enable []string
	for _, c := range changes {
		if c.Action == ActionEnable {
			enable = append(enable, c.Object)
		}
	}
	if len(enable) == 0 {
		return nil
	}
	defer s.Env.ForgetServices()
	return s.Env.Cloud.ServiceUsage.EnableServices(ctx, s.Env.Config.Project, enable)
}

func enabledSet(o Observed) map[string]bool {
	on := map[string]bool{}
	for _, svc := range o.(*APIsObserved).Enabled {
		on[svc] = true
	}
	return on
}
