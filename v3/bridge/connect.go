package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwsalmon/silo/v3/acp"
	"github.com/bwsalmon/silo/v3/gateway"
)

// ConnectOptions configures `silo connect`.
type ConnectOptions struct {
	SocketPath       string
	URL              string
	Token            string
	CLIsDir          string
	Schedule         string
	HomeDir          string
	StopGraceTimeout time.Duration
	Command          []string
	Stderr           io.Writer
}

func (o *ConnectOptions) resolveToken() (string, error) {
	if o.Token != "" {
		return strings.TrimSpace(o.Token), nil
	}
	if t := strings.TrimSpace(os.Getenv("SILO_AGENT_TOKEN")); t != "" {
		return t, nil
	}
	if credDir := os.Getenv("CREDENTIALS_DIRECTORY"); credDir != "" {
		if b, err := os.ReadFile(filepath.Join(credDir, "token")); err == nil {
			if t := strings.TrimSpace(string(b)); t != "" {
				return t, nil
			}
		}
	}
	return "", errors.New("missing agent token: pass --token or set SILO_AGENT_TOKEN")
}

func (o *ConnectOptions) dial(ctx context.Context) (FrameConn, string, error) {
	targetURL := o.URL
	sock := o.SocketPath
	if targetURL == "" && sock == "" {
		if envURL := os.Getenv("SILO_HUB_URL"); envURL != "" {
			if strings.HasPrefix(envURL, "unix://") {
				sock = strings.TrimPrefix(envURL, "unix://")
			} else if strings.HasPrefix(envURL, "ws://") || strings.HasPrefix(envURL, "wss://") ||
				strings.HasPrefix(envURL, "http://") || strings.HasPrefix(envURL, "https://") {
				targetURL = envURL
			} else {
				sock = envURL
			}
		}
	}
	if targetURL != "" {
		ws, err := DialWS(ctx, targetURL, nil)
		if err != nil {
			return nil, "", err
		}
		return ws, sock, nil
	}
	if sock == "" {
		if envSock := os.Getenv("SILO_BRIDGE_SOCKET"); envSock != "" {
			sock = envSock
		} else {
			sock = DefaultBridgeSocket
		}
	}
	sock = strings.TrimPrefix(sock, "unix://")
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, "", err
	}
	return newLineConn(conn, nil), sock, nil
}

// RunConnect runs `silo connect`: dials the bridge, presents the registration
// token, starts the ACP CLI in its own process group, streams ACP stdio
// bidirectionally, and handles stop signals and atomic 0600 secret writes.
func RunConnect(ctx context.Context, opts ConnectOptions) error {
	tok, err := opts.resolveToken()
	if err != nil {
		return err
	}
	if opts.StopGraceTimeout <= 0 {
		opts.StopGraceTimeout = 30 * time.Second
	}
	homeDir := opts.HomeDir
	if homeDir == "" {
		homeDir, _ = os.UserHomeDir()
	}

	fc, sockPath, err := opts.dial(ctx)
	if err != nil {
		return err
	}
	defer fc.Close()

	host, _ := os.Hostname()
	if err := sendFrame(fc, acp.Frame{
		Silo:  acp.ControlConnect,
		Token: tok,
		Host:  host,
		PID:   os.Getpid(),
	}); err != nil {
		return fmt.Errorf("sending connect handshake: %w", err)
	}

	rawAck, err := fc.ReadFrame()
	if err != nil {
		return fmt.Errorf("reading connect ack: %w", err)
	}
	var ack acp.Frame
	if err := json.Unmarshal(rawAck, &ack); err != nil {
		return fmt.Errorf("decoding connect ack: %w", err)
	}
	if ack.Err != "" {
		return errors.New(ack.Err)
	}
	if ack.Silo != acp.ControlConnected {
		return fmt.Errorf("unexpected handshake frame %q", ack.Silo)
	}

	argv, extraEnv, err := acp.ResolveCommand(opts.CLIsDir, ack.Runtime, opts.Command)
	if err != nil {
		_ = sendFrame(fc, acp.Frame{Silo: acp.ControlExit, Err: err.Error()})
		return err
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	env := os.Environ()
	for k, v := range extraEnv {
		env = append(env, k+"="+v)
	}
	env = append(env,
		"SILO_AGENT="+ack.Agent,
		"SILO_AGENT_TOKEN="+ack.SessionToken,
	)
	if sockPath != "" {
		env = append(env, "SILO_BRIDGE_SOCKET="+sockPath)
	}
	if homeDir != "" {
		env = append(env, "HOME="+homeDir)
	}
	cmd.Env = env
	if opts.Stderr != nil {
		cmd.Stderr = opts.Stderr
	} else {
		cmd.Stderr = os.Stderr
	}

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		_ = sendFrame(fc, acp.Frame{Silo: acp.ControlExit, Err: err.Error()})
		return err
	}
	pgid := cmd.Process.Pid

	var stopOnce sync.Once
	signalChild := func(mode string, timeoutMS int) {
		if mode == "kill" {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			return
		}
		stopOnce.Do(func() {
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
			grace := opts.StopGraceTimeout
			if timeoutMS > 0 {
				grace = time.Duration(timeoutMS) * time.Millisecond
			}
			time.AfterFunc(grace, func() {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			})
		})
	}

	// Stream CLI stdout -> bridge.
	stdoutDone := make(chan struct{})
	go func() {
		defer close(stdoutDone)
		br := bufio.NewReaderSize(stdoutPipe, 64<<10)
		for {
			var lineBuf []byte
			for {
				chunk, isPrefix, rerr := br.ReadLine()
				if rerr != nil {
					if len(lineBuf) > 0 {
						_ = fc.WriteFrame(lineBuf)
					}
					return
				}
				lineBuf = append(lineBuf, chunk...)
				if !isPrefix {
					break
				}
			}
			trimmed := bytes.TrimSpace(lineBuf)
			if len(trimmed) == 0 {
				continue
			}
			if err := fc.WriteFrame(trimmed); err != nil {
				return
			}
		}
	}()

	// Stream bridge -> CLI stdin + control messages.
	bridgeDone := make(chan struct{})
	go func() {
		defer close(bridgeDone)
		defer stdinPipe.Close()
		for {
			frameBytes, rerr := fc.ReadFrame()
			if rerr != nil {
				signalChild("graceful", 0)
				return
			}
			var ctrl struct {
				Silo string `json:"silo"`
			}
			_ = json.Unmarshal(frameBytes, &ctrl)
			if ctrl.Silo == "" {
				// Forward ACP JSON-RPC message to CLI stdin.
				if _, werr := stdinPipe.Write(append(bytes.TrimRight(frameBytes, "\n"), '\n')); werr != nil {
					return
				}
				continue
			}

			var f acp.Frame
			if err := json.Unmarshal(frameBytes, &f); err != nil {
				continue
			}
			switch f.Silo {
			case acp.ControlStop:
				signalChild(f.Mode, f.TimeoutMS)
			case acp.ControlSecretWrite:
				writtenPath, werr := WriteSecretFile(homeDir, f.Name, f.Path, []byte(f.Value))
				ackFrame := acp.Frame{
					Silo:  acp.ControlSecretAck,
					ReqID: f.ReqID,
					Name:  f.Name,
					Path:  writtenPath,
				}
				if werr != nil {
					ackFrame.Err = werr.Error()
				} else {
					ackFrame.Status = gateway.SecretStored
				}
				_ = sendFrame(fc, ackFrame)
			}
		}
	}()

	// Stop child if parent context is cancelled.
	ctxDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			signalChild("graceful", 0)
		case <-ctxDone:
		}
	}()

	<-stdoutDone
	waitErr := cmd.Wait()
	close(ctxDone)

	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	} else if waitErr != nil {
		exitCode = 1
	}
	_ = sendFrame(fc, acp.Frame{
		Silo: acp.ControlExit,
		Code: &exitCode,
	})
	_ = fc.Close()
	<-bridgeDone
	return nil
}

// WriteSecretFile atomically writes secret bytes with mode 0600 to targetPath
// (or ~/.config/silo/secrets/<name> when targetPath is empty), expanding ~/
// against homeDir and rejecting path traversal.
func WriteSecretFile(homeDir, name, targetPath string, data []byte) (string, error) {
	if name == "" {
		return "", errors.New("secret name is required")
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("invalid secret name %q", name)
		}
	}
	if targetPath == "" {
		targetPath = gateway.DefaultSecretDir + "/" + name
	}
	resolved := targetPath
	if rest, ok := strings.CutPrefix(targetPath, "~/"); ok {
		if homeDir == "" {
			var err error
			homeDir, err = os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("resolving home dir: %w", err)
			}
		}
		for _, seg := range strings.Split(rest, "/") {
			if seg == ".." {
				return "", fmt.Errorf("secret path %q must not contain ..", targetPath)
			}
		}
		resolved = filepath.Join(homeDir, filepath.FromSlash(rest))
	} else {
		if !filepath.IsAbs(targetPath) {
			return "", fmt.Errorf("secret path %q must be absolute or start with ~/", targetPath)
		}
		for _, seg := range strings.Split(targetPath, "/") {
			if seg == ".." {
				return "", fmt.Errorf("secret path %q must not contain ..", targetPath)
			}
		}
		resolved = filepath.Clean(targetPath)
	}

	dir := filepath.Dir(resolved)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	base := filepath.Base(resolved)
	tmp, err := os.CreateTemp(dir, "."+base+"-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, resolved); err != nil {
		return "", err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return resolved, nil
}
