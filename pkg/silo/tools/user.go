package tools

import (
	"fmt"
	"os/user"
	"strconv"
	"syscall"
)

// lookupUser is name's credential, with its groups, and its home. A
// static build reads /etc/passwd and /etc/group itself (os/user without
// cgo), which is what a tool VM has.
func lookupUser(name string) (*syscall.Credential, string, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, "", fmt.Errorf("the user %s that commands run as: %w", name, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, "", err
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, "", err
	}
	cred := &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	if gs, err := u.GroupIds(); err == nil {
		for _, g := range gs {
			if n, err := strconv.ParseUint(g, 10, 32); err == nil {
				cred.Groups = append(cred.Groups, uint32(n))
			}
		}
	}
	return cred, u.HomeDir, nil
}
