//go:build !linux

package local

import (
	"context"
	"errors"
	"time"

	"github.com/bwsalmon/silo/pkg/backend/local/netfence"
)

// HolderNFT is nft in silo-net through the namespace holder, which
// needs Linux.
func HolderNFT(state, nft string) netfence.NFT {
	return netfence.Nsenter(func() (int, error) { return 0, errors.New("the local backend needs Linux") }, nft)
}

// HolderMapping is the holder's id mapping, which needs Linux.
func HolderMapping(ctx context.Context, state string, timeout time.Duration) (string, error) {
	return "", errors.New("the local backend needs Linux")
}
