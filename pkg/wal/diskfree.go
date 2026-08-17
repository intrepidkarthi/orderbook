package wal

import "path/filepath"

// FreeBytes reports the space available on the filesystem holding the set at stem,
// and whether the question could be answered at all.
//
// It is a measurement, not a policy: nothing in this package calls it, because
// nothing in this package has a threshold to apply to the answer. cmd/obgw samples it
// once per checkpoint tick, off the matching goroutine, and owns every threshold in
// docs/LOG-ROTATION.md §6.2. Rotation deliberately does NOT sample it — rotateLocked
// runs on the matching goroutine under the writer's lock, and a statfs there would buy
// an earlier warning at the cost of putting a filesystem call in the append path. The
// reason it exists is in
// docs/LOG-ROTATION.md §6.1: before it, a full disk produced a venue that kept
// accepting orders, kept acknowledging them, kept matching them and stopped
// journalling, at fifty log lines a second with /readyz still green. Every
// acknowledgement after the first failed sync was a lie, and the venue was the only
// party that could have known.
//
// A caller that gets ok == false must not conclude the disk is healthy. It means the
// platform cannot answer, so the thresholds simply do not fire and the latched
// failure in Writer.Sync — which is not a threshold — is the whole of the protection
// there.
func FreeBytes(stem string) (int64, bool) {
	dir := filepath.Dir(stem)
	if dir == "" {
		dir = "."
	}
	return freeBytes(dir)
}
