package router

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// checkLoopback refuses a tool set whose processes listen anywhere but
// loopback: every TCP listener on its port, and every one its process
// group owns, must be on 127.0.0.0/8 or ::1. It reads /proc, so it only
// runs on Linux.
func checkLoopback(pgid, port int) error {
	owned := groupSockets(pgid)
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		ls, err := listeners(table)
		if err != nil {
			continue
		}
		for _, l := range ls {
			if (l.port == port || owned[l.inode]) && !l.ip.IsLoopback() {
				return fmt.Errorf("it listens on %s", net.JoinHostPort(l.ip.String(), strconv.Itoa(l.port)))
			}
		}
	}
	return nil
}

type listener struct {
	ip    net.IP
	port  int
	inode string
}

// listeners parses the LISTEN sockets of /proc/net/tcp or tcp6.
func listeners(path string) ([]listener, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []listener
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 || fields[3] != "0A" { // 0A is TCP_LISTEN
			continue
		}
		host, portHex, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		ip, err := procIP(host)
		if err != nil {
			continue
		}
		p, err := strconv.ParseUint(portHex, 16, 16)
		if err != nil {
			continue
		}
		out = append(out, listener{ip: ip, port: int(p), inode: fields[9]})
	}
	return out, sc.Err()
}

// procIP decodes /proc/net's address encoding: the address's 32-bit
// words in hex, each in host (little-endian) byte order.
func procIP(s string) (net.IP, error) {
	b, err := hex.DecodeString(s)
	if err != nil || (len(b) != 4 && len(b) != 16) {
		return nil, fmt.Errorf("bad address %q", s)
	}
	for i := 0; i < len(b); i += 4 {
		b[i], b[i+1], b[i+2], b[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	return net.IP(b), nil
}

// groupSockets returns the socket inodes held open by the members of
// process group pgid.
func groupSockets(pgid int) map[string]bool {
	inodes := map[string]bool{}
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || procPgrp(pid) != pgid {
			continue
		}
		fds, _ := os.ReadDir(filepath.Join("/proc", e.Name(), "fd"))
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join("/proc", e.Name(), "fd", fd.Name()))
			if err == nil && strings.HasPrefix(target, "socket:[") {
				inodes[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] = true
			}
		}
	}
	return inodes
}

// procPgrp is a process's group id, from /proc/<pid>/stat, or -1.
func procPgrp(pid int) int {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return -1
	}
	// The command name, in parentheses, may itself hold spaces.
	i := strings.LastIndexByte(string(data), ')')
	if i < 0 {
		return -1
	}
	fields := strings.Fields(string(data[i+1:]))
	if len(fields) < 3 {
		return -1
	}
	g, err := strconv.Atoi(fields[2]) // state, ppid, pgrp
	if err != nil {
		return -1
	}
	return g
}
