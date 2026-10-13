package local

import (
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
