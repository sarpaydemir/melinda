package main

import (
	"io/fs"
	"runtime/debug"
	"strconv"
	"strings"
)

// limitMemory sets the runtime's soft memory limit to 90% of the container's memory limit, read from the cgroup
// files under root, unless environ sets GOMEMLIMIT or the container has no limit. It returns the limit it set, or 0.
func limitMemory(environ []string, root fs.FS) int64 {
	for _, e := range environ {
		if v, ok := strings.CutPrefix(e, "GOMEMLIMIT="); ok && v != "" {
			return 0
		}
	}
	for _, name := range []string{"memory.max", "memory/memory.limit_in_bytes"} { // cgroup v2, v1
		b, err := fs.ReadFile(root, name)
		if err != nil {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if err != nil || n >= 1<<62 { // v2 writes max for no limit, v1 a number near 2^63
			return 0
		}
		limit := n / 10 * 9
		debug.SetMemoryLimit(limit)
		return limit
	}
	return 0
}
