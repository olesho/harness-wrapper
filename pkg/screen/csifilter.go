package screen

// csiFilter removes, from a PTY byte stream, the CSI sequences vt10x cannot
// interpret correctly, before they reach the emulator.
//
// vt10x recognizes only the "?" private marker and executes every final byte
// by its unmarked meaning. So:
//
//   - any sequence with the "<", "=" or ">" marker is misread: CSI > 4 ; 2 m
//     (xterm modifyOtherKeys) runs as an SGR reset, and the kitty keyboard
//     protocol's push (CSI > flags u), pop (CSI < n u) and set (CSI = flags ; m u)
//     run as restore-cursor;
//   - CSI ? u, the kitty keyboard protocol QUERY, also runs as restore-cursor.
//
// A restore-cursor that is not one teleports the cursor to whatever position
// was last saved — at a TUI's startup, the top of the screen — and every
// relative repaint after it lands in the wrong rows. claude-code 2.1.270 sends
// CSI ? u right after painting its folder-trust dialog, so the rendered screen
// stopped showing the highlight move at all. None of these sequences changes
// what is displayed, so dropping them is exact, not a heuristic.
//
// The filter is a small state machine so a sequence split across two writes
// is still recognized; a partial sequence at the end of a write is held back
// until the next one. Everything that is not one of these sequences passes
// through byte-for-byte, including every other CSI and every other escape.
type csiFilter struct {
	pending []byte // an incomplete CSI sequence carried over from the last write
}

// csiMax bounds a held-back sequence. Longer is not a sequence this filter
// drops, so it is passed through rather than buffered without limit.
const csiMax = 64

// filter returns p with the sequences described above removed, holding back
// an incomplete trailing CSI for the next call.
func (f *csiFilter) filter(p []byte) []byte {
	in := p
	if len(f.pending) > 0 {
		in = append(f.pending, p...)
		f.pending = nil
	}
	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); {
		if in[i] != 0x1b {
			out = append(out, in[i])
			i++
			continue
		}
		if i+1 >= len(in) {
			f.pending = append([]byte(nil), in[i:]...) // ESC at the very end
			break
		}
		if in[i+1] != '[' {
			out = append(out, in[i])
			i++
			continue
		}
		// CSI: parameter bytes 0x30–0x3F, intermediates 0x20–0x2F, final 0x40–0x7E.
		j := i + 2
		for j < len(in) && in[j] >= 0x20 && in[j] <= 0x3F {
			j++
		}
		if j >= len(in) {
			if len(in)-i <= csiMax {
				f.pending = append([]byte(nil), in[i:]...)
				break
			}
			out = append(out, in[i:]...)
			break
		}
		final := in[j]
		if final < 0x40 || final > 0x7E {
			// Not a well-formed CSI; hand it to the emulator untouched.
			out = append(out, in[i])
			i++
			continue
		}
		if dropCSI(in[i+2:j], final) {
			i = j + 1
			continue
		}
		out = append(out, in[i:j+1]...)
		i = j + 1
	}
	return out
}

// dropCSI reports whether the CSI with these parameter/intermediate bytes and
// final byte must not reach vt10x.
func dropCSI(params []byte, final byte) bool {
	if len(params) == 0 {
		return false
	}
	switch params[0] {
	case '<', '=', '>':
		return true
	case '?':
		return final == 'u'
	}
	return false
}
