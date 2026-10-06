package benchgate

import "fmt"

// digestVerdict is what the committed .digest file's rules say about a fresh replay
// (docs/BENCH-GATE.md §3.5). The rules hang off matching.SemanticsVersion because
// that number is the repository's statement of whether matching behaviour was meant
// to change.
type digestVerdict int

const (
	digestOK      digestVerdict = iota // the file and the replay agree
	digestWrite                        // regeneration is allowed and was asked for
	digestRefused                      // a failure; the error says which rule
)

// checkDigest applies the rules. recorded is nil when there is no file yet.
func checkDigest(recorded *DigestFile, current int, tapeSHA [32]byte, got Digest, update bool) (digestVerdict, error) {
	if recorded == nil {
		if update {
			return digestWrite, nil
		}
		return digestRefused, fmt.Errorf("no committed digest; write it once with BENCHGATE_UPDATE=1")
	}
	if recorded.TapeSHA256 != tapeSHA {
		return digestRefused, fmt.Errorf("the digest names tape sha256 %x, the tape is %x", recorded.TapeSHA256, tapeSHA)
	}
	switch {
	case recorded.Semantics > current:
		return digestRefused, fmt.Errorf("the digest was written at SemanticsVersion %d, above the current %d: a downgrade", recorded.Semantics, current)
	case recorded.Semantics < current:
		if update {
			return digestWrite, nil
		}
		return digestRefused, fmt.Errorf("SemanticsVersion moved %d -> %d; regenerate the digest with BENCHGATE_UPDATE=1 "+
			"(that is part of every bump, docs/SEMANTICS-VERSION.md)", recorded.Semantics, current)
	}
	same := recorded.Core == got.Core && recorded.Full == got.Full
	if update {
		// The same rule semcheck's golden follows: at an unchanged SemanticsVersion
		// the digest is not regenerated, it is explained.
		return digestRefused, fmt.Errorf("BENCHGATE_UPDATE refuses to write at an unchanged SemanticsVersion %d", current)
	}
	if !same {
		return digestRefused, fmt.Errorf("the engine's output on the bench tape changed at an unchanged SemanticsVersion %d. "+
			"Either this is a defect, or matching behaviour changed on purpose: then extend the semcheck corpus, "+
			"sabotage-measure it, bump SemanticsVersion, and regenerate", current)
	}
	return digestOK, nil
}
