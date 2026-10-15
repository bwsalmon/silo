package hostcap

import (
	"errors"
	"os"
	"syscall"
)

func memTotal() (int64, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	mb := MemTotalMB(string(b))
	if mb == 0 {
		return 0, errors.New("/proc/meminfo has no MemTotal")
	}
	return mb, nil
}

func diskGB(dataDir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dataDir, &st); err != nil {
		return 0, err
	}
	return int64((st.Blocks * uint64(st.Bsize)) >> 30), nil
}
