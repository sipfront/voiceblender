package api

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/recording"
)

// fakeMixer stands in for the room mixer, which stopAll only ever uses to
// detach record taps.
type fakeMixer struct{}

func (fakeMixer) SetParticipantRecordTap(string, io.Writer) {}
func (fakeMixer) ClearParticipantRecordTap(string)          {}

// TestMultiChannelStopAll_SurvivesDiscardedLeg pins the blast radius of a single
// leg's capture failure. A discarded capture leaves nothing at the path Stop
// reports, and the merge refuses to open a file that is not there — so storing
// that path anyway meant one participant's failure destroyed every other
// participant's audio along with it. The healthy legs must still merge, and the
// dropped one must be named rather than silently vanishing.
func TestMultiChannelStopAll_SurvivesDiscardedLeg(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	mc := &multiChannelState{
		active: true,
		// Backdated so the merge spans a real duration and actually writes frames.
		startTime:    time.Now().Add(-time.Second),
		sampleRate:   8000,
		dir:          dir,
		recorders:    map[string]*recording.Recorder{},
		pipes:        map[string]*pipeWriter{},
		files:        map[string]string{},
		joinOffsets:  map[string]time.Duration{},
		leaveOffsets: map[string]time.Duration{},
		log:          slog.Default(),
	}

	// A healthy leg: a finite reader, so the capture ends at EOF and publishes.
	// Waiting for it here is what makes the test deterministic — stopAll would
	// otherwise cancel it before its first frame, which discards the capture and
	// omits the leg from the merge, leaving nothing for this test to prove.
	good := recording.NewRecorder(slog.Default())
	if _, err := good.StartAt(ctx, bytes.NewReader(make([]byte, 16000)), dir, 8000, ""); err != nil {
		t.Fatalf("start good leg: %v", err)
	}
	good.Wait()
	if !good.Published() {
		t.Fatal("precondition: the good leg did not publish, so this test proves nothing")
	}

	// A failing leg: a stereo companion that cannot be drained without blocking
	// is a real, reachable capture error, so this staging file is discarded.
	bad := recording.NewRecorder(slog.Default())
	if _, err := bad.StartStereo(ctx, bytes.NewReader(nil), bytes.NewReader(nil), dir, 8000, ""); err != nil {
		t.Fatalf("start bad leg: %v", err)
	}
	bad.Wait()
	if bad.Published() {
		t.Fatal("precondition: the bad leg published, so this test proves nothing")
	}

	mc.recorders["good"] = good
	mc.recorders["bad"] = bad
	mc.participantOrder = []string{"good", "bad"}

	res, err := mc.stopAll(fakeMixer{})
	if err != nil {
		t.Fatalf("stopAll lost the whole room over one discarded leg: %v", err)
	}
	if _, ok := res.Channels["good"]; !ok {
		t.Errorf("the healthy leg is missing from the merge; channels = %v", res.Channels)
	}
	if _, ok := res.Channels["bad"]; ok {
		t.Errorf("the discarded leg was given a channel, but it has no audio; channels = %v", res.Channels)
	}
	if got := res.OmittedLegs; len(got) != 1 || got[0] != "bad" {
		t.Errorf("OmittedLegs = %v, want [bad] — a partial recording must name what it lost", got)
	}
}

// TestMultiChannelStopAll_AllLegsDiscardedFails is the other half of the
// contract: salvaging survivors must not degrade into reporting an empty room as
// a success. With nothing to merge, the stop has to fail loudly.
func TestMultiChannelStopAll_AllLegsDiscardedFails(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	mc := &multiChannelState{
		active:       true,
		startTime:    time.Now().Add(-time.Second),
		sampleRate:   8000,
		dir:          dir,
		recorders:    map[string]*recording.Recorder{},
		pipes:        map[string]*pipeWriter{},
		files:        map[string]string{},
		joinOffsets:  map[string]time.Duration{},
		leaveOffsets: map[string]time.Duration{},
		log:          slog.Default(),
	}

	bad := recording.NewRecorder(slog.Default())
	if _, err := bad.StartStereo(ctx, bytes.NewReader(nil), bytes.NewReader(nil), dir, 8000, ""); err != nil {
		t.Fatalf("start bad leg: %v", err)
	}
	bad.Wait()
	if bad.Published() {
		t.Fatal("precondition: the bad leg published, so this test proves nothing")
	}

	mc.recorders["bad"] = bad
	mc.participantOrder = []string{"bad"}

	if _, err := mc.stopAll(fakeMixer{}); err == nil {
		t.Fatal("stopAll reported success though no leg published any audio")
	}
}

// TestStopRoomRecordingIfEmpty_PublishesOmittedLegs covers the auto-stop exit.
// A room recording has two ways to finish — the API stop and this one, taken
// when the last leg leaves — and only the API stop is reachable from a request,
// so the merge result reaching the event on this path has no other coverage.
// Naming the lost leg is the whole point of salvaging the survivors: a listener
// that is told a partial recording is complete has been misinformed.
func TestStopRoomRecordingIfEmpty_PublishesOmittedLegs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := newTestServer(t)

	const roomID = "auto-stop-room"
	if _, err := s.RoomMgr.Create(roomID, "test-app", 8000); err != nil {
		t.Fatalf("create room: %v", err)
	}
	// The room is empty, which is the precondition this exit tests for; a leg
	// still present would make it return before publishing anything.

	// The room mix recorder: cleanupRoomRecording reports no recording at all
	// without one, and the auto-stop returns silently.
	mix := recording.NewRecorder(slog.Default())
	if _, err := mix.StartAt(ctx, bytes.NewReader(make([]byte, 16000)), dir, 8000, ""); err != nil {
		t.Fatalf("start room mix recorder: %v", err)
	}
	roomRecorders.Lock()
	roomRecorders.m[roomID] = mix
	roomRecorders.Unlock()
	t.Cleanup(func() {
		roomRecorders.Lock()
		delete(roomRecorders.m, roomID)
		roomRecorders.Unlock()
	})

	good := recording.NewRecorder(slog.Default())
	if _, err := good.StartAt(ctx, bytes.NewReader(make([]byte, 16000)), dir, 8000, ""); err != nil {
		t.Fatalf("start good leg: %v", err)
	}
	good.Wait()
	if !good.Published() {
		t.Fatal("precondition: the good leg did not publish, so this test proves nothing")
	}

	bad := recording.NewRecorder(slog.Default())
	if _, err := bad.StartStereo(ctx, bytes.NewReader(nil), bytes.NewReader(nil), dir, 8000, ""); err != nil {
		t.Fatalf("start bad leg: %v", err)
	}
	bad.Wait()
	if bad.Published() {
		t.Fatal("precondition: the bad leg published, so this test proves nothing")
	}

	mc := &multiChannelState{
		active:           true,
		startTime:        time.Now().Add(-time.Second),
		sampleRate:       8000,
		dir:              dir,
		recorders:        map[string]*recording.Recorder{"good": good, "bad": bad},
		pipes:            map[string]*pipeWriter{},
		files:            map[string]string{},
		joinOffsets:      map[string]time.Duration{},
		leaveOffsets:     map[string]time.Duration{},
		participantOrder: []string{"good", "bad"},
		log:              slog.Default(),
	}
	roomMultiChannel.Lock()
	roomMultiChannel.m[roomID] = mc
	roomMultiChannel.Unlock()
	t.Cleanup(func() {
		roomMultiChannel.Lock()
		delete(roomMultiChannel.m, roomID)
		roomMultiChannel.Unlock()
	})

	var got *events.RecordingFinishedData
	unsub := s.Bus.Subscribe(func(e events.Event) {
		if e.Type != events.RecordingFinished {
			return
		}
		if d, ok := e.Data.(*events.RecordingFinishedData); ok {
			got = d
		}
	})
	defer unsub()

	s.stopRoomRecordingIfEmpty(roomID)

	if got == nil {
		t.Fatal("the auto-stop published no recording.finished, so this test proves nothing")
	}
	if _, ok := got.Channels["good"]; !ok {
		t.Errorf("the healthy leg is missing from the event; channels = %v", got.Channels)
	}
	if names := got.OmittedLegs; len(names) != 1 || names[0] != "bad" {
		t.Errorf("OmittedLegs = %v, want [bad] — the auto-stop reported a partial recording as complete", names)
	}
}

// gatedMixer holds a leg inside stopLeg, at the point where the leg has already
// been taken out of the recorder map but its file has not been published yet.
// ClearParticipantRecordTap is the first thing stopLeg does after it drops the
// lock, which makes it the seam for the interleaving this test is about.
type gatedMixer struct {
	leg     string
	entered chan struct{}
	release chan struct{}
}

func (g gatedMixer) SetParticipantRecordTap(string, io.Writer) {}

func (g gatedMixer) ClearParticipantRecordTap(legID string) {
	if legID != g.leg {
		return
	}
	close(g.entered)
	<-g.release
}

// TestMultiChannelStopAll_WaitsForALegAlreadyStopping is the bridged-call case.
//
// stopLeg removes the leg from the recorder map under the lock, then finalizes
// outside it and publishes into files only at the end. stopAll snapshots the
// recorder map and then reads files, so a leg stopping concurrently was in
// neither: not waited for, not merged, and reported as a capture that had been
// discarded when it had not. Both legs of a bridged call get their BYE in the
// same instant — one leaves the room while the other's departure stops the whole
// recording — so the window is not a rare one, and every application-server call
// came out of it with a single-channel "multi-channel" file.
func TestMultiChannelStopAll_WaitsForALegAlreadyStopping(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	mc := &multiChannelState{
		active:       true,
		startTime:    time.Now().Add(-time.Second),
		sampleRate:   8000,
		dir:          dir,
		recorders:    map[string]*recording.Recorder{},
		pipes:        map[string]*pipeWriter{},
		files:        map[string]string{},
		joinOffsets:  map[string]time.Duration{},
		leaveOffsets: map[string]time.Duration{},
		log:          slog.Default(),
	}

	// Two healthy legs, both already at EOF: what is being tested is the
	// bookkeeping around their stop, not the capture itself.
	for _, leg := range []string{"a", "b"} {
		rec := recording.NewRecorder(slog.Default())
		if _, err := rec.StartAt(ctx, bytes.NewReader(make([]byte, 16000)), dir, 8000, ""); err != nil {
			t.Fatalf("start leg %s: %v", leg, err)
		}
		rec.Wait()
		if !rec.Published() {
			t.Fatalf("precondition: leg %s did not publish, so this test proves nothing", leg)
		}
		mc.recorders[leg] = rec
	}
	mc.participantOrder = []string{"a", "b"}

	gate := gatedMixer{leg: "b", entered: make(chan struct{}), release: make(chan struct{})}
	go mc.stopLeg("b", gate)
	<-gate.entered // "b" is out of the recorder map and not yet in files

	// Held long enough that a stopAll which does not wait cannot see b's file by
	// luck. A stopAll which does wait releases at once and takes no longer than
	// this either way.
	time.AfterFunc(200*time.Millisecond, func() { close(gate.release) })

	res, err := mc.stopAll(gate)
	if err != nil {
		t.Fatalf("stopAll: %v", err)
	}
	if len(res.OmittedLegs) != 0 {
		t.Errorf("a leg that was mid-stop was reported lost: %v", res.OmittedLegs)
	}
	for _, leg := range []string{"a", "b"} {
		if _, ok := res.Channels[leg]; !ok {
			t.Errorf("leg %s is missing from the merge; channels = %v", leg, res.Channels)
		}
	}
}
