package forge

import (
	"crypto/sha256"
	"encoding/hex"
)

// BranchName returns a deterministic branch name for one proposal:
// jetsam/drop-<fingerprint>.
//
// Deterministic in fingerprint so re-running against an unchanged proposal
// reuses the branch, which is what the idempotence check (an existing open
// PR on this branch) depends on. When the proposal changes -- a different
// set of metrics next run -- the name changes too, so a new PR is opened
// rather than force-pushing over one a human may be mid-review on.
func BranchName(fingerprint string) string {
	sum := sha256.Sum256([]byte(fingerprint))
	return "jetsam/drop-" + hex.EncodeToString(sum[:])[:12]
}
