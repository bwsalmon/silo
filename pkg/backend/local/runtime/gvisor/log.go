package gvisor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"
)

// LogArg is the subcommand that runs LogMain: `silod runtime-log`.
const LogArg = "runtime-log"

// Entry is one line of a container's log file, in Docker's json-file
// format, so that `docker logs` reads it the way dockerd would have
// written it.
type Entry struct {
	Log    string    `json:"log"`
	Stream string    `json:"stream"`
	Time   time.Time `json:"time"`
}

// LogMain is `silod runtime-log`, a container's log copier: it reads the
// workload's stdout and stderr FIFOs, which the runsc shim writes, and
// appends each line to the container's log file. The shim only speaks
// FIFOs (gVisor's shim has no logging binary or file scheme), so
// something has to read them for as long as the workload runs. The
// copier is a process of its own, in its own session, so that it
// outlives silod as the shim does: a restarted silod finds the log
// complete, and adopts the copier by its pid file
// (local-backend.md §12, "Restart and adoption", step 4).
//
// With --stdin, it also holds the stdin FIFO open for writing, so the
// workload's stdin stays open between attaches, as Docker's does
// without StdinOnce.
func LogMain(args []string) error {
	fs := flag.NewFlagSet(LogArg, flag.ContinueOnError)
	stdout := fs.String("stdout", "", "the stdout FIFO")
	stderr := fs.String("stderr", "", "the stderr FIFO")
	stdin := fs.String("stdin", "", "the stdin FIFO, held open")
	logf := fs.String("log", "", "the log file")
	pidf := fs.String("pid", "", "the pid file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *logf == "" || *stdout == "" {
		return errors.New("runtime-log: --log and --stdout are required")
	}
	out, err := os.OpenFile(*logf, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if *stdin != "" {
		// O_RDWR doesn't block on a FIFO, and keeps a writer.
		in, err := os.OpenFile(*stdin, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer in.Close()
	}
	if *pidf != "" {
		if err := os.WriteFile(*pidf, []byte(fmt.Sprint(os.Getpid())), 0o600); err != nil {
			return err
		}
	}
	w := &logWriter{f: out}
	var wg sync.WaitGroup
	for _, s := range []struct{ name, path string }{{"stdout", *stdout}, {"stderr", *stderr}} {
		if s.path == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Opening for reading blocks until the shim opens the
			// FIFO for writing, and the read ends when the workload's
			// last writer goes.
			f, err := os.Open(s.path)
			if err != nil {
				w.write("stderr", []byte("silo: "+err.Error()+"\n"))
				return
			}
			defer f.Close()
			copyLines(w, s.name, f)
		}()
	}
	wg.Wait()
	if *pidf != "" {
		os.Remove(*pidf)
	}
	return nil
}

// maxLine is the longest line kept as one entry; longer ones are split,
// as dockerd splits at 16 KiB.
const maxLine = 16 << 10

func copyLines(w *logWriter, stream string, r io.Reader) {
	br := bufio.NewReaderSize(r, maxLine)
	for {
		line, err := br.ReadSlice('\n')
		if len(line) > 0 {
			w.write(stream, line)
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return
		}
	}
}

type logWriter struct {
	mu sync.Mutex
	f  *os.File
}

func (w *logWriter) write(stream string, b []byte) {
	line, _ := json.Marshal(Entry{Log: string(b), Stream: stream, Time: time.Now().UTC()})
	w.mu.Lock()
	defer w.mu.Unlock()
	w.f.Write(append(line, '\n'))
}

// ReadLog splits a log file into the workload's stdout and stderr so
// far. A file that doesn't exist yet is an empty log.
func ReadLog(path string) (stdout, stderr []byte, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	var o, e bytes.Buffer
	err = ScanLog(context.Background(), f, func(en Entry) {
		if en.Stream == "stderr" {
			e.WriteString(en.Log)
		} else {
			o.WriteString(en.Log)
		}
	})
	return o.Bytes(), e.Bytes(), err
}

// ScanLog calls fn for each entry in r. A torn last line, which a
// copier still writing leaves, is skipped.
func ScanLog(ctx context.Context, r io.Reader, fn func(Entry)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4*maxLine)
	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var en Entry
		if json.Unmarshal(sc.Bytes(), &en) != nil {
			continue
		}
		fn(en)
	}
	return sc.Err()
}

// alive is whether pid is a running process.
func alive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}
