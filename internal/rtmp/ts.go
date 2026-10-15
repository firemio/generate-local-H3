package rtmp

// A minimal MPEG-TS reader: enough to find, in one HLS segment, where each video access unit starts and
// which are keyframes, so a clip boundary that the broadcaster's muxer left inside a segment can still
// be delivered in two writes with the overlay changed in between, at the right frame (follower.deliver).

const tsPacket = 188

// videoPES is one video access unit (one frame) of a segment: the start of its PES packet.
type videoPES struct {
	Off int     // byte offset of the first TS packet of the PES packet
	PTS float64 // seconds from the segment's first video frame (33-bit wrap handled)
	Key bool    // random-access indicator set (ffmpeg's muxer sets it on every keyframe it writes)
}

// tsVideo scans data (a whole segment) and returns its video access units in stream order. Anything
// unreadable (a truncated tail, a PES without a PTS) yields fewer units, never a wrong one.
func tsVideo(data []byte) []videoPES {
	var (
		out   []videoPES
		first int64 = -1 // PTS of the first video PES (90 kHz)
	)
	for off := 0; off+tsPacket <= len(data); off += tsPacket {
		p := data[off : off+tsPacket]
		if p[0] != 0x47 {
			break // lost sync: what follows is not worth a guess
		}
		if p[1]&0x40 == 0 {
			continue // no payload_unit_start: a continuation
		}
		afc := (p[3] >> 4) & 3
		payload := 4
		rai := false
		if afc&2 != 0 { // adaptation field present
			alen := int(p[4])
			if alen > 0 {
				rai = p[5]&0x40 != 0
			}
			payload += 1 + alen
		}
		if afc&1 == 0 || payload+14 > len(p) {
			continue
		}
		pes := p[payload:]
		if pes[0] != 0 || pes[1] != 0 || pes[2] != 1 {
			continue // PSI, not a PES
		}
		if sid := pes[3]; sid < 0xE0 || sid > 0xEF {
			continue // audio or private stream
		}
		if pes[7]&0x80 == 0 {
			continue // no PTS
		}
		b := pes[9:14]
		pts := int64(b[0]>>1&7)<<30 | int64(b[1])<<22 | int64(b[2]>>1)<<15 | int64(b[3])<<7 | int64(b[4]>>1)
		if first < 0 {
			first = pts
		}
		d := pts - first
		switch {
		case d < -1<<32:
			d += 1 << 33
		case d > 1<<32:
			d -= 1 << 33
		}
		out = append(out, videoPES{Off: off, PTS: float64(d) / 90000, Key: rai})
	}
	return out
}

// flushOffset is where a write may pause so that the overlay can change for the clip whose first frame
// is pes[i]: just past the first TS packet of the PES packet flushFrames frames later (size, the
// segment's end, when the segment has fewer).
//
// Why not at the keyframe itself: after the last byte of the old clip, ffmpeg still holds its last
// frames — the mpegts demuxer keeps a PES packet until the next one starts, the H.264 parser keeps one
// access unit until it sees the next, and the decoder's reorder buffer keeps has_b_frames (2 for the
// library's clips: High profile x264 with B-pyramid). They only come out as the new clip's frames go
// in, one for one. Handing ffmpeg the first packet of frame i+flushFrames completes frame i+flushFrames-1
// and pushes the last old frame out of the decoder (measured: with the split right at the keyframe the
// text led the picture by 3 frames inside a segment and 4 at a segment start; with this one it lands on
// the new clip's first frame).
func flushOffset(pes []videoPES, i, size int) int {
	if i+flushFrames < len(pes) {
		return min(size, pes[i+flushFrames].Off+tsPacket)
	}
	return size
}
