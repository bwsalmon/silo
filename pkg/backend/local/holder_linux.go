package local

import (
	"context"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/netfence"
	"github.com/bwsalmon/silo/pkg/backend/local/nsd"
)

// HolderNFT is nft (a path, or "nft") run in silo-net through the
// namespace holder whose state file is state (nsd.state), which is
// read again at each call: a holder that restarted is found at its new
// pid.
func HolderNFT(state, nft string) netfence.NFT {
	return netfence.Nsenter(func() (int, error) {
		st, err := nsd.ReadState(state)
		return st.Pid, err
	}, nft)
}

// HolderMapping waits up to timeout for the holder whose state file is
// state, and returns its id mapping, as BindMapping takes it.
func HolderMapping(ctx context.Context, state string, timeout time.Duration) (string, error) {
	st, err := nsd.WaitState(ctx, state, timeout)
	if err != nil {
		return "", err
	}
	m, err := nsd.ReadMapping(st.Pid)
	if err != nil {
		return "", err
	}
	return m.String(), nil
}
