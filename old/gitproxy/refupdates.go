// Adapted from github.com/bwsalmon/grain pkg/gitproxy/refupdates.go at 5789160c875aa7abb6b4978c3fa337ea6f985a94, Apache License 2.0.

package gitproxy

// Which refs a push may update. Authorize answers per repository, which
// is all a fetch can be answered at (a sandbox allowed a repository gets
// every object in it), but a push names the refs it moves in plain text
// at the very start of its body, ahead of the packfile:
//
//	<old-oid> SP <new-oid> SP <refname> [NUL <capabilities>] LF
//
// one pkt-line per ref, ended by a flush-pkt. That command list is
// read here and checked before the request is forwarded (Policy.RefUpdates),
// so a caller that may push to acme/widgets may still push only the refs
// its policy names -- whatever branch protection the host happens to
// have, and whatever the credential the proxy forwards with could do
// there.
//
// The parser fails closed: a body whose command list it cannot read in
// full is refused, never forwarded on the guess that it is harmless. It
// reads only the command list, never the packfile after it, so there is
// no pack parsing here to get subtly wrong.

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// RefUpdate is one command of a push: move Ref from Old to New. An
// all-zero New deletes Ref; an all-zero Old creates it.
type RefUpdate struct {
	Old, New, Ref string
}

// pushCheck is what GitProxy.refuseRefUpdates proves fast-forwards
// with, beyond the command list: the decoded request body, for the
// packfile a fast-forward is shown from (pack.go), and the upstream, for
// the commits it already has (ancestry.go). Its fastForward method is the
// FastForward a Policy is handed.
type pushCheck struct {
	// body opens the decoded request body from its start. It is opened
	// only if a ref asks for the pack, and read as a stream: a push
	// spooled to disk is never held in memory whole.
	body func() (io.Reader, error)
	// upstream fetches, from the repository being pushed to, the commits
	// reachable from wants and not from have: where a walk that leaves
	// the pack carries on (ancestry.go). Nil when there is nobody to ask.
	upstream func(wants []string, have string, hashLen int) (packCommits, error)

	commits    packCommits
	commitsErr error
	read       bool
}

// packCommits reads the push's pack once, however many refs ask.
func (c *pushCheck) packCommits(hashLen int) (packCommits, error) {
	if c == nil || c.body == nil {
		return nil, errors.New("no packfile in hand")
	}
	if !c.read {
		c.read = true
		body, err := c.body()
		if err == nil {
			r := bufio.NewReaderSize(body, 64<<10)
			if err = skipToPack(r); err == nil {
				c.commits, err = readPackStream(r, hashLen)
			}
		}
		c.commitsErr = err
	}
	return c.commits, c.commitsErr
}

// fastForward reports whether newTip is shown to descend from oldTip:
// by the commits the push carries, or -- where a walk through them runs
// out, as it does at once when the remote already has newTip -- by the
// commits the remote has (ancestry.go). why says what stopped a proof
// that was not made, when that is more than the walk running out.
func (c *pushCheck) fastForward(newTip, oldTip string) (ok bool, why string) {
	hashLen := len(newTip) / 2
	commits, err := c.packCommits(hashLen)
	if errors.Is(err, errNoPack) {
		commits, err = packCommits{}, nil
	}
	if err != nil {
		return false, ""
	}
	if commits.descends(newTip, oldTip) {
		return true, ""
	}
	if c.upstream == nil {
		return false, ""
	}
	wants, ok := commits.frontier(newTip, maxFrontierCommits)
	if !ok || len(wants) == 0 {
		return false, ""
	}
	fetched, err := c.upstream(wants, oldTip, hashLen)
	if err != nil {
		return false, err.Error()
	}
	for _, w := range wants {
		if fetched.descends(w, oldTip) {
			return true, ""
		}
	}
	return false, ""
}

// isZeroID is the all-zero object name a command uses for "no ref".
func isZeroID(id string) bool { return strings.Trim(id, "0") == "" }

// UpdateKind is what one update does to its ref: "delete", "create" or
// "update".
func UpdateKind(u RefUpdate) string {
	switch {
	case isZeroID(u.New):
		return "delete"
	case isZeroID(u.Old):
		return "create"
	default:
		return "update"
	}
}

func shortCommit(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// maxCommandListBytes bounds how much of a push is read to find its
// command list, and how much of a gzip-encoded push is inflated at all. A
// command is a few hundred bytes at most, so a list that does not end
// inside this is not one git sent.
const maxCommandListBytes = 16 << 20

// decodedPush is a receive-pack request body as the client wrote it:
// body itself, or inflated -- as far as maxCommandListBytes, which is as
// much as the command list can need -- when the client gzip-encoded it.
func decodedPush(body io.Reader, contentEncoding string) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(contentEncoding)) {
	case "", "identity":
		return body, nil
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(body)
		if err != nil {
			return nil, fmt.Errorf("reading gzip-encoded push: %w", err)
		}
		return io.LimitReader(zr, maxCommandListBytes), nil
	default:
		return nil, fmt.Errorf("unsupported Content-Encoding %q on a push", contentEncoding)
	}
}

// commandList returns the command list at the start of a receive-pack
// request body, decoded: its pkt-lines up to and including the flush-pkt
// that ends it, read off body and no further, so the packfile behind it
// is never read here. A list that is truncated, malformed or longer than
// maxCommandListBytes comes back as far as it was read, for
// parseReceivePack to refuse.
func commandList(body io.Reader, contentEncoding string) ([]byte, error) {
	r, err := decodedPush(body, contentEncoding)
	if err != nil {
		return nil, err
	}
	var list []byte
	for len(list) < maxCommandListBytes {
		var head [4]byte
		if _, err := io.ReadFull(r, head[:]); err != nil {
			return list, endOfList(err)
		}
		list = append(list, head[:]...)
		n, err := strconv.ParseUint(string(head[:]), 16, 16)
		if err != nil || n < 4 {
			// The flush-pkt that ends the list, or a length
			// parseReceivePack refuses.
			return list, nil
		}
		payload := make([]byte, n-4)
		read, err := io.ReadFull(r, payload)
		list = append(list, payload[:read]...)
		if err != nil {
			return list, endOfList(err)
		}
	}
	return list, nil
}

// endOfList is a read error commandList returns: none for a body that
// simply ended, which parseReceivePack refuses as a truncated list.
func endOfList(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return nil
	}
	return fmt.Errorf("reading push: %w", err)
}

// ParseReceivePackCommands reads the command list at the start of a
// git-receive-pack request body: any shallow lines, then one command per
// pkt-line up to the flush-pkt that ends the list. Anything else -- a
// truncated list, a malformed command, a signed push's certificate,
// which carries its commands inside the certificate -- is an error, and
// the caller refuses the push.
func ParseReceivePackCommands(body []byte) ([]RefUpdate, error) {
	updates, _, err := parseReceivePack(body)
	return updates, err
}

// parseReceivePack is ParseReceivePackCommands that also returns the
// capabilities the client sent after the NUL on its first command --
// which say how it wants a refusal answered (rejectPush).
func parseReceivePack(body []byte) (updates []RefUpdate, capabilities []string, err error) {
	for rest := body; ; {
		if len(rest) < 4 {
			return nil, nil, errors.New("push ended before its command list did")
		}
		n, err := strconv.ParseUint(string(rest[:4]), 16, 16)
		if err != nil {
			return nil, nil, fmt.Errorf("malformed pkt-line length %q in push", rest[:4])
		}
		if n == 0 {
			return updates, capabilities, nil
		}
		if n < 4 || int(n) > len(rest) {
			return nil, nil, fmt.Errorf("malformed pkt-line length %d in push", n)
		}
		line := string(rest[4:n])
		rest = rest[n:]
		if i := strings.IndexByte(line, 0); i >= 0 {
			if len(updates) == 0 {
				capabilities = strings.Fields(strings.TrimSuffix(line[i+1:], "\n"))
			}
			line = line[:i]
		}
		line = strings.TrimSuffix(line, "\n")
		if strings.HasPrefix(line, "shallow ") && len(updates) == 0 {
			continue
		}
		if strings.HasPrefix(line, "push-cert") {
			return nil, nil, errors.New("signed pushes are not supported through this git proxy; push without --signed")
		}
		fields := strings.Split(line, " ")
		if len(fields) != 3 || !isObjectID(fields[0]) || !isObjectID(fields[1]) || fields[2] == "" {
			return nil, nil, fmt.Errorf("malformed ref-update command %q in push", line)
		}
		updates = append(updates, RefUpdate{Old: fields[0], New: fields[1], Ref: fields[2]})
	}
}

// isObjectID is a SHA-1 or SHA-256 object name in hex.
func isObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// rejectPush answers a push whose command list was read but refused the
// way receive-pack itself refuses an update: a 200 carrying report-status,
// "ng <ref> <reason>" for every command, so `git push` prints
//
//	! [remote rejected] main -> main (<reason>)
//
// and the whole reason as "remote:" lines, rather than the bare "HTTP
// 403" it prints for any refused POST without ever showing its body.
// Nothing was unpacked and no ref moved: the packfile never left this
// proxy. A client that did not ask for report-status cannot read that
// answer, so it gets the plain-text 403 instead (false).
func rejectPush(updates []RefUpdate, capabilities []string, reason string) (ProxyResponse, bool) {
	var reportStatus, sideband bool
	for _, c := range capabilities {
		switch c {
		case "report-status", "report-status-v2":
			reportStatus = true
		case "side-band", "side-band-64k":
			sideband = true
		}
	}
	if !reportStatus || len(updates) == 0 {
		return ProxyResponse{}, false
	}
	// An ng reason is one line of a pkt-line; the full text goes out on
	// the progress band below, where the client prints every line.
	short := strings.Join(strings.Fields(reason), " ")
	if len(short) > 200 {
		short = short[:200] + "..."
	}
	var report bytes.Buffer
	writePkt(&report, "unpack ok\n")
	for _, u := range updates {
		writePkt(&report, fmt.Sprintf("ng %s %s\n", u.Ref, short))
	}
	report.WriteString("0000")

	var body bytes.Buffer
	if sideband {
		// Band 2 is progress, printed as "remote: <line>"; band 1 carries
		// the report itself. Each band packet is well under the 1000
		// bytes plain side-band allows.
		for _, line := range strings.Split(strings.TrimSpace(reason), "\n") {
			for len(line) > 900 {
				writePkt(&body, "\x02"+line[:900])
				line = line[900:]
			}
			writePkt(&body, "\x02"+line+"\n")
		}
		for data := report.Bytes(); len(data) > 0; {
			n := min(len(data), 900)
			writePkt(&body, "\x01"+string(data[:n]))
			data = data[n:]
		}
		body.WriteString("0000")
	} else {
		body.Write(report.Bytes())
	}
	return ProxyResponse{
		Status: 200,
		Headers: map[string]string{
			"Content-Type":  "application/x-git-receive-pack-result",
			"Cache-Control": "no-cache",
		},
		Body: body.Bytes(),
	}, true
}

// writePkt appends payload to b as one pkt-line.
func writePkt(b *bytes.Buffer, payload string) {
	fmt.Fprintf(b, "%04x%s", len(payload)+4, payload)
}
