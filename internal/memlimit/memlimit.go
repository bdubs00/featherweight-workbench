// Package memlimit gives the Go runtime the memory limit of its container.
// The collector of Go lets the heap grow to twice what stays live, and it
// does not know the limit of the container, so a front that decodes a
// large upload can pass that limit with garbage that the collector would
// free, and the node then kills it. With a soft limit, the collector works
// harder as the heap nears the limit.
package memlimit

import (
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// Share is the part of the limit of the container that the runtime may
// use before the collector works harder. The rest is for the memory that
// the runtime does not count, such as the mapped files and the buffers of
// the kernel of the node.
const Share = 0.8

// The files that hold the memory limit of the cgroup of the process: cgroup
// v2 first, then cgroup v1.
var limitFiles = []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"}

// noLimit is the bound above which a cgroup v1 value means no limit.
const noLimit = 1 << 50

// FromCgroup sets the soft memory limit of the runtime to Share of the
// memory limit of the cgroup of the process, unless the variable
// GOMEMLIMIT sets a limit already. It returns the limit that it set, or 0
// when it set none: outside a container, or for a container without a
// limit.
func FromCgroup() int64 {
	return apply(os.Getenv("GOMEMLIMIT"), limitFiles, debug.SetMemoryLimit)
}

func apply(env string, files []string, set func(int64) int64) int64 {
	if strings.TrimSpace(env) != "" {
		return 0
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if err != nil || n <= 0 || n >= noLimit {
			// "max" in cgroup v2, or a value that means no limit.
			return 0
		}
		limit := int64(float64(n) * Share)
		set(limit)
		return limit
	}
	return 0
}
