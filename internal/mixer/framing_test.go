package mixer

import (
	"io"
	"log/slog"
	"testing"
)

// chunkReader hands out PCM in the sizes a peer's packetisation would produce,
// one Read per chunk, then EOF. A source is under no obligation to deliver
// exactly one mix frame per Read: ptime is advisory, nothing here parses it, and
// a peer may change it mid-call without renegotiating.
type chunkReader struct {
	chunks [][]byte
	i      int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.i >= len(r.chunks) {
		return 0, io.EOF
	}
	c := r.chunks[r.i]
	n := copy(p, c)
	if n < len(c) {
		r.chunks[r.i] = c[n:]
		return n, nil
	}
	r.i++
	return n, nil
}

// pcmChunks splits total bytes of recognisable PCM into chunks of the given
// sizes, cycling through them. Each sample carries its own index so a test can
// prove not just how much audio arrived but that none of it was dropped or
// reordered.
func pcmChunks(total int, sizes ...int) ([][]byte, []byte) {
	all := make([]byte, 0, total)
	for i := 0; len(all) < total; i++ {
		all = append(all, byte(i&0xff), byte((i>>8)&0xff))
	}
	all = all[:total]

	var chunks [][]byte
	for off, k := 0, 0; off < len(all); k++ {
		size := sizes[k%len(sizes)]
		if off+size > len(all) {
			size = len(all) - off
		}
		chunks = append(chunks, all[off:off+size])
		off += size
	}
	return chunks, all
}

// collectFrames runs the read loop against src and returns every frame it
// queued. The channel is deep enough that nothing is dropped, so the test sees
// exactly what the loop produced.
func collectFrames(t *testing.T, m *Mixer, src io.Reader) [][]byte {
	t.Helper()
	p := &Participant{
		ID:       "p",
		Reader:   src,
		incoming: make(chan []byte, 4096),
		done:     make(chan struct{}),
	}
	done := make(chan struct{})
	go func() {
		m.readLoop(p, make(chan struct{}))
		close(done)
	}()
	<-done
	close(p.incoming)

	var frames [][]byte
	for f := range p.incoming {
		frames = append(frames, f)
	}
	return frames
}

func TestReadLoop_FramesAreWholeWhateverThePacketisation(t *testing.T) {
	m := New(slog.New(slog.DiscardHandler), 16000)
	frame := m.FrameSizeBytes() // 640 B = 20 ms at 16 kHz

	cases := []struct {
		name  string
		sizes []int
	}{
		// The case that broke live transcription: a peer switched to 10 ms
		// packets after a hold. One Read per packet used to mean one queued
		// frame per packet, so the mix loop — which consumes exactly one entry
		// per 20 ms tick — took half the audio and the drop-oldest branch threw
		// the rest away.
		{"10ms packets", []int{frame / 2}},
		{"20ms packets", []int{frame}},
		{"30ms packets", []int{frame * 3 / 2}},
		{"40ms packets", []int{frame * 2}},
		// A peer that changes packetisation mid-stream, which is what a hold and
		// retrieve produced in practice.
		{"20ms then 10ms", []int{frame, frame, frame / 2, frame / 2}},
		// Sizes that divide into no whole number of frames at all.
		{"ragged", []int{100, 640, 32, 1000, 2}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunks, want := pcmChunks(frame*20, tc.sizes...)
			frames := collectFrames(t, m, &chunkReader{chunks: chunks})

			var got []byte
			for i, f := range frames {
				if len(f) != frame {
					t.Fatalf("frame %d is %d bytes, want %d — a short frame reaches "+
						"the mix and every tap as if it were a whole one", i, len(f), frame)
				}
				got = append(got, f...)
			}

			// Everything except the last partial frame must arrive, in order.
			// Losing audio here is invisible downstream: it looks like the party
			// simply said less.
			if len(got) != len(want) {
				t.Fatalf("assembled %d bytes of audio, want %d", len(got), len(want))
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("audio differs at byte %d: got %d, want %d — frames were "+
						"dropped or reordered", i, got[i], want[i])
				}
			}
		})
	}
}

// A source whose packets are smaller than one frame is a fact worth reporting:
// it is legal, it costs bandwidth and CPU, and it used to be the difference
// between a working transcript and silence.
func TestReadLoop_CountsShortReads(t *testing.T) {
	m := New(slog.New(slog.DiscardHandler), 16000)
	frame := m.FrameSizeBytes()

	p := &Participant{
		ID:       "p",
		incoming: make(chan []byte, 4096),
		done:     make(chan struct{}),
	}

	chunks, _ := pcmChunks(frame*4, frame/2)
	p.Reader = &chunkReader{chunks: chunks}
	m.readLoop(p, make(chan struct{}))

	if got := p.framesRead.Load(); got != 4 {
		t.Errorf("framesRead = %d, want 4 whole frames from 8 half-frame packets", got)
	}
	if got := p.shortReads.Load(); got != 4 {
		t.Errorf("shortReads = %d, want 4 — one per packet that did not fill a frame", got)
	}
}

// A frame that is still incomplete when the source ends is dropped rather than
// queued short. At most one Ptime of audio is lost, at hangup, where it cannot
// matter — and the mix never sees a frame it would have to interpret.
func TestReadLoop_DiscardsThePartialFrameAtEndOfStream(t *testing.T) {
	m := New(slog.New(slog.DiscardHandler), 16000)
	frame := m.FrameSizeBytes()

	chunks, _ := pcmChunks(frame+frame/2, frame/2)
	frames := collectFrames(t, m, &chunkReader{chunks: chunks})

	if len(frames) != 1 {
		t.Fatalf("queued %d frames, want 1 whole frame with the remainder dropped", len(frames))
	}
	if len(frames[0]) != frame {
		t.Errorf("frame is %d bytes, want %d", len(frames[0]), frame)
	}
}
