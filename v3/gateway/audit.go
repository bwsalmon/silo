package gateway

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// AuditEntry is one line of /var/log/silo/audit.jsonl (design §4.8).
type AuditEntry struct {
	Seq     int64           `json:"seq"`
	Time    time.Time       `json:"time"`
	Actor   string          `json:"actor"`
	Event   string          `json:"event"`
	Payload json.RawMessage `json:"payload,omitempty"`
	// Prev is the hex sha256 of the previous line, without its newline; ""
	// on the first entry.
	Prev string `json:"prev"`
}

// Audit events written by the gateway.
const (
	EvGatewayStart   = "gateway.start"
	EvRegister       = "agent.register"
	EvConnect        = "agent.connect"
	EvDisconnect     = "agent.disconnect"
	EvStop           = "agent.stop"
	EvMessage        = "agent.message"
	EvDeliver        = "agent.deliver"
	EvPageRegister   = "page.register"
	EvPageUnregister = "page.unregister"
	EvSecretRequest  = "secret.request"
	EvSecretProvide  = "secret.provide"
	EvSecretReject   = "secret.reject"
	EvAuthDeny       = "auth.deny"
)

// AuditLog is the append-only, hash-chained audit log. Only the gateway
// writes it.
type AuditLog struct {
	mu    sync.Mutex
	f     *os.File
	path  string
	clock Clock
	seq   int64
	prev  string
}

// OpenAuditLog opens (creating it, mode 0600) the log at path with
// O_APPEND, and recovers the sequence number and chain head from its last
// line. Run as root, it makes the file root-owned and, with appendOnly, sets
// the append-only inode flag where the filesystem allows (chattr +a);
// failing to set the flag is not an error.
func OpenAuditLog(path string, clock Clock, appendOnly bool) (*AuditLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	// An append-only file refuses chmod and chown even to root, so change
	// only what is wrong.
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if fi.Mode().Perm() != 0o600 {
		if err := f.Chmod(0o600); err != nil {
			f.Close()
			return nil, err
		}
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 && (st.Uid != 0 || st.Gid != 0) {
		if err := f.Chown(0, 0); err != nil {
			f.Close()
			return nil, err
		}
	}
	if appendOnly {
		setAppendOnly(f)
	}
	a := &AuditLog{f: f, path: path, clock: clock}
	last, err := lastLine(path)
	if err != nil {
		f.Close()
		return nil, err
	}
	if last != nil {
		var e AuditEntry
		if err := json.Unmarshal(last, &e); err != nil {
			f.Close()
			return nil, fmt.Errorf("audit log %s: last line is not an entry: %w", path, err)
		}
		a.seq = e.Seq
		a.prev = lineHash(last)
	}
	return a, nil
}

func lineHash(line []byte) string {
	h := sha256.Sum256(line)
	return hex.EncodeToString(h[:])
}

// lastLine returns the last non-empty line of the file, or nil.
func lastLine(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	// Read backwards in growing chunks until a whole line is in view.
	size := st.Size()
	for chunk := int64(64 << 10); ; chunk *= 4 {
		off := max(size-chunk, 0)
		buf := make([]byte, size-off)
		if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
			return nil, err
		}
		buf = bytes.TrimRight(buf, "\n")
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			return buf[i+1:], nil
		}
		if off == 0 {
			if len(buf) == 0 {
				return nil, nil
			}
			return buf, nil
		}
	}
}

// Append writes one entry. The payload is marshalled as JSON.
func (a *AuditLog) Append(actor, event string, payload any) (AuditEntry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := AuditEntry{Seq: a.seq + 1, Time: a.clock.Now(), Actor: actor, Event: event, Prev: a.prev}
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return e, err
		}
		e.Payload = b
	}
	line, err := json.Marshal(e)
	if err != nil {
		return e, err
	}
	if _, err := a.f.Write(append(line, '\n')); err != nil {
		return e, err
	}
	if err := a.f.Sync(); err != nil {
		return e, err
	}
	a.seq = e.Seq
	a.prev = lineHash(line)
	return e, nil
}

// Close closes the log.
func (a *AuditLog) Close() error { return a.f.Close() }

// Read returns up to limit entries with seq > after.
func (a *AuditLog) Read(after int64, limit int) ([]AuditEntry, error) {
	f, err := os.Open(a.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := []AuditEntry{}
	err = scanLines(f, func(line []byte) error {
		var e AuditEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return err
		}
		if e.Seq > after && len(out) < limit {
			out = append(out, e)
		}
		return nil
	})
	return out, err
}

// VerifyResult is the outcome of walking the hash chain.
type VerifyResult struct {
	OK        bool   `json:"ok"`
	Entries   int64  `json:"entries"`
	FirstBad  int64  `json:"first_bad_seq,omitempty"`
	BadReason string `json:"reason,omitempty"`
}

// Verify walks the chain and reports the first entry whose seq or prev does
// not follow from the line before it.
func (a *AuditLog) Verify() (VerifyResult, error) {
	f, err := os.Open(a.path)
	if err != nil {
		return VerifyResult{}, err
	}
	defer f.Close()
	return verifyChain(f)
}

var errStop = errors.New("stop")

func verifyChain(r io.Reader) (VerifyResult, error) {
	res := VerifyResult{OK: true}
	var prev string
	var seq int64
	err := scanLines(r, func(line []byte) error {
		var e AuditEntry
		bad := ""
		if err := json.Unmarshal(line, &e); err != nil {
			bad = "not a JSON entry"
		} else if e.Seq != seq+1 {
			bad = fmt.Sprintf("seq %d follows %d", e.Seq, seq)
		} else if e.Prev != prev {
			bad = "prev does not match the previous line"
		}
		if bad != "" {
			res.OK, res.FirstBad, res.BadReason = false, seq+1, bad
			return errStop
		}
		seq, prev = e.Seq, lineHash(line)
		res.Entries++
		return nil
	})
	if errors.Is(err, errStop) {
		err = nil
	}
	return res, err
}

func scanLines(r io.Reader, fn func([]byte) error) error {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		line = bytes.TrimRight(line, "\n")
		if len(line) > 0 {
			if ferr := fn(line); ferr != nil {
				return ferr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// setAppendOnly sets FS_APPEND_FL on f (chattr +a), best effort.
func setAppendOnly(f *os.File) {
	const (
		fsIocGetFlags = 0x80086601
		fsIocSetFlags = 0x40086602
		fsAppendFl    = 0x00000020
	)
	var flags int32
	fd := f.Fd()
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, fsIocGetFlags, uintptr(unsafe.Pointer(&flags))); e != 0 {
		return
	}
	if flags&fsAppendFl != 0 {
		return
	}
	flags |= fsAppendFl
	syscall.Syscall(syscall.SYS_IOCTL, fd, fsIocSetFlags, uintptr(unsafe.Pointer(&flags)))
}
