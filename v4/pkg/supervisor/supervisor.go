package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/bwsalmon/silo/v4/pkg/auth"
	"github.com/bwsalmon/silo/v4/pkg/quota"
)

var (
	ErrAgentNotFound = errors.New("agent not found")
)

// AgentLifecycle defines execution behavior.
type AgentLifecycle string

const (
	LifecycleOneShot    AgentLifecycle = "one_shot"
	LifecyclePersistent AgentLifecycle = "persistent"
)

// AgentStatus represents the current state of an agent.
type AgentStatus string

const (
	StatusPending   AgentStatus = "PENDING"
	StatusRunning   AgentStatus = "RUNNING"
	StatusCompleted AgentStatus = "COMPLETED"
	StatusFailed    AgentStatus = "FAILED"
	StatusStopped   AgentStatus = "STOPPED"
)

// AgentSpec defines the launch parameters for a sub-agent.
type AgentSpec struct {
	ID             string
	ParentID       string
	Name           string
	Description    string
	Prompt         string
	Lifecycle      AgentLifecycle
	ServiceAccount string
	AllowedCAB     []string
	ShareRepo      bool
	AllowVMTools   bool
	InitialMemory  map[string]interface{}
	Command        []string
	Env            []string
	Timeout        time.Duration
}

// AgentProcess represents a supervised agent instance.
type AgentProcess struct {
	Spec      AgentSpec
	Status    AgentStatus
	StartTime time.Time
	EndTime   time.Time
	ExitCode  int
	Restarts  int
	Output    string

	cmd    *exec.Cmd
	cancel context.CancelFunc
}

// Supervisor manages child processes, crash restarts, and graceful teardowns.
type Supervisor struct {
	mu            sync.RWMutex
	agents        map[string]*AgentProcess
	ledger        *quota.Ledger
	tokenProvider auth.TokenProvider
	baseDir       string
	repoDir       string
	shuttingDown  bool
}

// NewSupervisor creates a supervisor instance.
func NewSupervisor(ledger *quota.Ledger, tokenProvider auth.TokenProvider, baseDir, repoDir string) *Supervisor {
	return &Supervisor{
		agents:        make(map[string]*AgentProcess),
		ledger:        ledger,
		tokenProvider: tokenProvider,
		baseDir:       baseDir,
		repoDir:       repoDir,
	}
}

// Spawn launches an agent process under supervision.
func (s *Supervisor) Spawn(ctx context.Context, spec AgentSpec) (*AgentProcess, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.shuttingDown {
		return nil, errors.New("supervisor is shutting down")
	}

	if _, exists := s.agents[spec.ID]; exists {
		return nil, fmt.Errorf("agent %s already exists", spec.ID)
	}

	proc := &AgentProcess{
		Spec:      spec,
		Status:    StatusPending,
		StartTime: time.Now(),
	}

	s.agents[spec.ID] = proc
	go s.runLoop(spec.ID)

	return proc, nil
}

func (s *Supervisor) runLoop(agentID string) {
	backoff := 100 * time.Millisecond
	maxBackoff := 10 * time.Second

	for {
		s.mu.RLock()
		proc, exists := s.agents[agentID]
		shuttingDown := s.shuttingDown
		s.mu.RUnlock()

		if !exists || shuttingDown {
			return
		}

		if proc.Status == StatusStopped || proc.Status == StatusCompleted {
			return
		}

		// Run one execution turn
		exitCode, _ := s.executeAgent(proc)

		s.mu.Lock()
		if proc.Status == StatusStopped || s.shuttingDown {
			s.mu.Unlock()
			return
		}
		proc.ExitCode = exitCode
		proc.EndTime = time.Now()

		if proc.Spec.Lifecycle == LifecycleOneShot {
			if exitCode == 0 {
				proc.Status = StatusCompleted
			} else {
				proc.Status = StatusFailed
			}
			s.ledger.ChildTerminated(agentID)
			s.mu.Unlock()
			return
		}

		// Persistent Lifecycle: Restart loop
		proc.Restarts++
		proc.Status = StatusRunning
		s.mu.Unlock()

		if shuttingDown {
			return
		}

		time.Sleep(backoff)
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (s *Supervisor) executeAgent(proc *AgentProcess) (int, error) {
	var ctx context.Context
	var cancel context.CancelFunc

	if proc.Spec.Timeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), proc.Spec.Timeout)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}

	s.mu.Lock()
	if proc.Status == StatusStopped || s.shuttingDown {
		s.mu.Unlock()
		return 0, nil
	}
	proc.cancel = cancel
	proc.Status = StatusRunning

	// Fallback to echo / mock command if empty (for testability)
	cmdArgs := proc.Spec.Command
	if len(cmdArgs) == 0 {
		cmdArgs = []string{"sleep", "0.01"}
	}

	cmd := exec.CommandContext(ctx, cmdArgs[0], cmdArgs[1:]...)
	// Set process group so we can cleanly kill all children in cascading teardown
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), proc.Spec.Env...)
	proc.cmd = cmd
	s.mu.Unlock()

	err := cmd.Run()

	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	return exitCode, err
}

// Stop terminates an agent and cascades SIGTERM to all child agents.
func (s *Supervisor) Stop(agentID string) (int, error) {
	s.mu.Lock()
	proc, exists := s.agents[agentID]
	if !exists {
		s.mu.Unlock()
		return 0, ErrAgentNotFound
	}
	proc.Status = StatusStopped
	if proc.cancel != nil {
		proc.cancel()
	}
	s.killProcessGroup(proc.cmd)
	s.mu.Unlock()

	// Release children from quota ledger and reap them
	reapedIDs := s.ledger.ReleaseSubtree(agentID)
	for _, childID := range reapedIDs {
		s.mu.Lock()
		if childProc, ok := s.agents[childID]; ok {
			childProc.Status = StatusStopped
			if childProc.cancel != nil {
				childProc.cancel()
			}
			s.killProcessGroup(childProc.cmd)
		}
		s.mu.Unlock()
	}

	return len(reapedIDs), nil
}

func (s *Supervisor) killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// Kill entire process group
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		time.Sleep(10 * time.Millisecond)
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	} else {
		_ = cmd.Process.Kill()
	}
}

// SetOutput records final task output for a one-shot agent.
func (s *Supervisor) SetOutput(agentID, output string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	proc, exists := s.agents[agentID]
	if !exists {
		return ErrAgentNotFound
	}
	proc.Output = output
	return nil
}

// GetAgent returns a snapshot copy of an agent process.
func (s *Supervisor) GetAgent(agentID string) (*AgentProcess, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	proc, exists := s.agents[agentID]
	if !exists {
		return nil, ErrAgentNotFound
	}

	cp := *proc
	return &cp, nil
}

// ListChildren returns snapshots of all direct children of an agent.
func (s *Supervisor) ListChildren(parentID string) []*AgentProcess {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var list []*AgentProcess
	for _, proc := range s.agents {
		if proc.Spec.ParentID == parentID {
			cp := *proc
			list = append(list, &cp)
		}
	}
	return list
}

// Shutdown reaps all running agents.
func (s *Supervisor) Shutdown() {
	s.mu.Lock()
	s.shuttingDown = true
	for _, proc := range s.agents {
		proc.Status = StatusStopped
		if proc.cancel != nil {
			proc.cancel()
		}
		s.killProcessGroup(proc.cmd)
	}
	s.mu.Unlock()
}
