//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/tts"
)

// pcmTTSProvider returns a fixed block of PCM, so a recording test needs no API key
// and no network. What is synthesized does not matter here; that it reaches a channel
// does.
type pcmTTSProvider struct{ audio []byte }

func (p *pcmTTSProvider) Synthesize(_ context.Context, _ string, _ tts.Options) (*tts.Result, error) {
	return &tts.Result{
		Audio:    io.NopCloser(bytes.NewReader(p.audio)),
		MimeType: "audio/pcm;rate=16000",
	}, nil
}

// recordedRoom is the shape all three tests start from: two legs bridged in one room
// with a multi-channel recording running, and a TTS provider that needs no credential.
func recordedRoom(t *testing.T, name string) (inst *testInstance, roomID, leg1, leg2 string) {
	t.Helper()
	instA := newTestInstance(t, name+"-a")
	instB := newTestInstance(t, name+"-b")
	instA.apiSrv.TTS = &pcmTTSProvider{audio: onesecPCM()}

	leg1, _ = establishCall(t, instA, instB)
	leg2, _ = establishCall(t, instA, instB)

	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{})
	if roomResp.StatusCode != http.StatusCreated {
		t.Fatalf("create room: %d", roomResp.StatusCode)
	}
	var rm roomView
	decodeJSON(t, roomResp, &rm)

	for _, legID := range []string{leg1, leg2} {
		resp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID),
			map[string]interface{}{"leg_id": legID})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("add leg %s: %d", legID, resp.StatusCode)
		}
		resp.Body.Close()
		id := legID
		instA.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
			return e.Data.GetLegID() == id
		}, 3*time.Second)
	}

	recResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/record", instA.baseURL(), rm.ID),
		map[string]interface{}{"multi_channel": true})
	if recResp.StatusCode != http.StatusOK {
		t.Fatalf("start recording: %d", recResp.StatusCode)
	}
	recResp.Body.Close()
	instA.collector.waitForMatch(t, events.RecordingStarted, func(e events.Event) bool {
		return e.Data.GetRoomID() == rm.ID
	}, 3*time.Second)

	return instA, rm.ID, leg1, leg2
}

// finishedRecording hangs both legs up and returns what the room's recording reported.
func finishedRecording(t *testing.T, inst *testInstance, roomID string, legs ...string) *events.RecordingFinishedData {
	t.Helper()
	for _, legID := range legs {
		httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", inst.baseURL(), legID)).Body.Close()
	}
	var finished events.Event
	inst.collector.waitForMatch(t, events.RecordingFinished, func(e events.Event) bool {
		if e.Data.GetRoomID() != roomID {
			return false
		}
		finished = e
		return true
	}, 10*time.Second)

	data, ok := finished.Data.(*events.RecordingFinishedData)
	if !ok {
		t.Fatalf("recording.finished carried %T", finished.Data)
	}
	return data
}

// A line synthesized into a room gets its own channel, for the same reason a file
// played into one does: the mix already contains it, and attributing it to a party's
// track would say they said it.
func TestMultiChannel_RoomTTSGetsItsOwnChannel(t *testing.T) {
	inst, roomID, leg1, leg2 := recordedRoom(t, "mc-roomtts")

	resp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/tts", inst.baseURL(), roomID),
		map[string]interface{}{"text": "attention please", "voice": "v", "api_key": "k", "record": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("say it into the room: %d", resp.StatusCode)
	}
	var said struct {
		TTSID string `json:"tts_id"`
	}
	decodeJSON(t, resp, &said)
	if said.TTSID == "" {
		t.Fatal("no tts id came back")
	}
	inst.collector.waitForMatch(t, events.TTSFinished, func(e events.Event) bool {
		d, ok := e.Data.(*events.TTSFinishedData)
		return ok && d.TTSID == said.TTSID
	}, 10*time.Second)

	data := finishedRecording(t, inst, roomID, leg1, leg2)
	if _, present := data.Channels[said.TTSID]; !present {
		t.Fatalf("the utterance has no channel of its own; channels = %v", data.Channels)
	}
	// The announcement is an addition, never a replacement.
	for _, legID := range []string{leg1, leg2} {
		if _, present := data.Channels[legID]; !present {
			t.Errorf("leg %s lost its channel; channels = %v", legID, data.Channels)
		}
	}
	if len(data.OmittedLegs) != 0 {
		t.Errorf("nothing may be reported lost: %v", data.OmittedLegs)
	}
	if !strings.HasSuffix(data.MultiChannelFile, ".wav") {
		t.Errorf("multi_channel_file = %q", data.MultiChannelFile)
	}
}

// A line spoken to one leg gets its own channel too, and stays out of the mix.
//
// This is the case the flag exists for. A one-sided announcement is written to that
// leg's own audio path, past the mixer, so only that party hears it — the mix is what
// the call sounded like to the room, and audio only one party heard does not belong in
// it. Without a channel of its own such an announcement was in no file at all: the
// recording held two people answering a question nobody could hear being asked.
func TestMultiChannel_LegTTSGetsItsOwnChannel(t *testing.T) {
	inst, roomID, leg1, leg2 := recordedRoom(t, "mc-legtts")

	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/tts", inst.baseURL(), leg1),
		map[string]interface{}{"text": "for you only", "voice": "v", "api_key": "k", "record": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("say it to the leg: %d", resp.StatusCode)
	}
	var said struct {
		TTSID string `json:"tts_id"`
	}
	decodeJSON(t, resp, &said)
	if said.TTSID == "" {
		t.Fatal("no tts id came back")
	}
	inst.collector.waitForMatch(t, events.TTSFinished, func(e events.Event) bool {
		d, ok := e.Data.(*events.TTSFinishedData)
		return ok && d.TTSID == said.TTSID
	}, 10*time.Second)

	data := finishedRecording(t, inst, roomID, leg1, leg2)
	if _, present := data.Channels[said.TTSID]; !present {
		t.Fatalf("the leg utterance has no channel of its own; channels = %v", data.Channels)
	}
	for _, legID := range []string{leg1, leg2} {
		if _, present := data.Channels[legID]; !present {
			t.Errorf("leg %s lost its channel; channels = %v", legID, data.Channels)
		}
	}
	if len(data.OmittedLegs) != 0 {
		t.Errorf("nothing may be reported lost: %v", data.OmittedLegs)
	}
}

// An utterance that did not ask to be recorded stays out of the file, which is what
// every caller written before `record` existed gets.
func TestMultiChannel_TTSIsNotRecordedUnlessAsked(t *testing.T) {
	inst, roomID, leg1, leg2 := recordedRoom(t, "mc-nott")

	for _, target := range []string{
		fmt.Sprintf("%s/v1/rooms/%s/tts", inst.baseURL(), roomID),
		fmt.Sprintf("%s/v1/legs/%s/tts", inst.baseURL(), leg1),
	} {
		resp := httpPost(t, target, map[string]interface{}{
			"text": "unrecorded", "voice": "v", "api_key": "k"})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("say it at %s: %d", target, resp.StatusCode)
		}
		resp.Body.Close()
	}
	time.Sleep(500 * time.Millisecond)

	data := finishedRecording(t, inst, roomID, leg1, leg2)
	if len(data.Channels) != 2 {
		t.Errorf("only the two legs have channels, got %v", data.Channels)
	}
	for id := range data.Channels {
		if strings.HasPrefix(id, "tts-") {
			t.Errorf("an utterance nobody asked to record got a channel: %v", data.Channels)
		}
	}
}
