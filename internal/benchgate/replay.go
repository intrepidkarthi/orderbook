package benchgate

// The two correctness replays. Neither runs inside a benchmark: hashing and naming
// cost more than matching does, and a timed loop must not measure them.

// DigestEngine replays a tape through pkg/matching and digests what it produced.
func DigestEngine(tp *Tape) (Digest, error) { return digestEngine(tp, false) }

func digestEngine(tp *Tape, keep bool) (Digest, error) {
	d := newEngineDriver(tp.MaxOrders, len(tp.Cmds))
	dg := newDigester(tp.FileSHA256, keep)
	for _, c := range tp.Cmds {
		o, err := d.apply(c)
		if err != nil {
			return Digest{}, err
		}
		if err := dg.command(o); err != nil {
			return Digest{}, err
		}
	}
	t, err := d.terminal()
	if err != nil {
		return Digest{}, err
	}
	return dg.finish(t), nil
}

// DigestRefmatch replays a tape through internal/refmatch and digests what it
// produced.
func DigestRefmatch(tp *Tape) (Digest, error) { return digestRefmatch(tp, false) }

func digestRefmatch(tp *Tape, keep bool) (Digest, error) {
	d := newRefDriver(tp.MaxOrders, len(tp.Cmds))
	n := &refNamer{pos: map[int64]uint64{}}
	dg := newDigester(tp.FileSHA256, keep)
	for _, c := range tp.Cmds {
		o, err := d.outcome(c, n)
		if err != nil {
			return Digest{}, err
		}
		if err := dg.command(o); err != nil {
			return Digest{}, err
		}
	}
	t, err := d.terminal(n)
	if err != nil {
		return Digest{}, err
	}
	return dg.finish(t), nil
}
