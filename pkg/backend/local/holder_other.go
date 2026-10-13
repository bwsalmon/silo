//go:build !linux

package local

import (
	"errors"

	"github.com/bwsalmon/silo/pkg/backend/local/netfence"
)

// HolderNFT is nft in silo-net through the namespace holder, which
// needs Linux.
func HolderNFT(state, nft string) netfence.NFT {
	return netfence.Nsenter(func() (int, error) { return 0, errors.New("the local backend needs Linux") }, nft)
}
