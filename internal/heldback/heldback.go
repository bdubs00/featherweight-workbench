// Package heldback picks the runs that a duty holds back from Know-how
// (docs/KNOWHOW.md section 6.6). A held-back run carries the metadata
// fw.knowhow: off, so the kernel gives it none of what the agent learned:
// no notes, no memory tools, and no routine. The front then compares the
// held-back runs with the normal runs of the same duties. The choice is a
// stable hash of the run id, so every replica and every retry of a start
// decides the same for the same run.
package heldback

import (
	"crypto/sha256"
	"encoding/binary"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Pick reports whether the run with the id is held back at the share,
// from 0 to 1: the first 53 bits of the SHA-256 of the id, as a fraction
// of 1, fall below the share. A share of 0 or less, and an empty id, hold
// nothing back.
func Pick(id string, share float64) bool {
	if share <= 0 || id == "" {
		return false
	}
	sum := sha256.Sum256([]byte(id))
	x := float64(binary.BigEndian.Uint64(sum[:8])>>11) / (1 << 53)
	return x < share
}

// Mark adds fw.knowhow: off to the metadata of a request whose run Pick
// holds back at the share, and reports whether it did.
func Mark(req *fw.RunRequest, share float64) bool {
	if req == nil || !Pick(req.ID, share) {
		return false
	}
	if req.Metadata == nil {
		req.Metadata = map[string]string{}
	}
	req.Metadata[fw.MetaKnowhow] = fw.KnowhowOff
	return true
}
