//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

// Audio played into a room gets its own channel in a multi-channel recording: the mix
// already contains it, and attributing it to a party's track would say they said it.
func TestMultiChannel_RoomPlaybackGetsItsOwnChannel(t *testing.T) {
	instA := newTestInstance(t, "mc-play-a")
	instB := newTestInstance(t, "mc-play-b")

	leg1, _ := establishCall(t, instA, instB)
	leg2, _ := establishCall(t, instA, instB)

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

	// A tone needs no file server.
	playResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/play", instA.baseURL(), rm.ID),
		map[string]interface{}{"tone": "dial", "record": true})
	if playResp.StatusCode != http.StatusOK {
		t.Fatalf("play into the room: %d", playResp.StatusCode)
	}
	var play struct {
		PlaybackID string `json:"playback_id"`
	}
	decodeJSON(t, playResp, &play)
	if play.PlaybackID == "" {
		t.Fatal("no playback id came back")
	}
	instA.collector.waitForMatch(t, events.PlaybackStarted, func(e events.Event) bool {
		return e.Data.GetRoomID() == rm.ID
	}, 3*time.Second)
	time.Sleep(300 * time.Millisecond)

	// An announcement ends long before the call does.
	httpDelete(t, fmt.Sprintf("%s/v1/rooms/%s/play/%s", instA.baseURL(), rm.ID,
		play.PlaybackID)).Body.Close()
	time.Sleep(200 * time.Millisecond)

	for _, legID := range []string{leg1, leg2} {
		httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), legID)).Body.Close()
	}

	var finished events.Event
	instA.collector.waitForMatch(t, events.RecordingFinished, func(e events.Event) bool {
		if e.Data.GetRoomID() != rm.ID {
			return false
		}
		finished = e
		return true
	}, 10*time.Second)

	data, ok := finished.Data.(*events.RecordingFinishedData)
	if !ok {
		t.Fatalf("recording.finished carried %T", finished.Data)
	}
	if _, present := data.Channels[play.PlaybackID]; !present {
		t.Fatalf("the playback has no channel of its own; channels = %v", data.Channels)
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

// Audio played to one leg gets its own channel too, and stays out of the mix.
//
// A leg playback is written to that leg's own audio path, past the mixer, so only that
// party hears it — which is the point of a one-sided announcement, and the reason it
// cannot be a room playback. The mix is what the call sounded like to the room, so
// audio only one party heard does not belong in it; the per-party file is where it can
// be kept without saying the other party heard it.
func TestMultiChannel_LegPlaybackGetsItsOwnChannel(t *testing.T) {
	instA := newTestInstance(t, "mc-legplay-a")
	instB := newTestInstance(t, "mc-legplay-b")

	leg1, _ := establishCall(t, instA, instB)
	leg2, _ := establishCall(t, instA, instB)

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

	playResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/play", instA.baseURL(), leg1),
		map[string]interface{}{"tone": "dial", "record": true})
	if playResp.StatusCode != http.StatusOK {
		t.Fatalf("play to the leg: %d", playResp.StatusCode)
	}
	var play struct {
		PlaybackID string `json:"playback_id"`
	}
	decodeJSON(t, playResp, &play)
	if play.PlaybackID == "" {
		t.Fatal("no playback id came back")
	}
	time.Sleep(400 * time.Millisecond)
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/play/%s", instA.baseURL(), leg1,
		play.PlaybackID)).Body.Close()
	time.Sleep(200 * time.Millisecond)

	for _, legID := range []string{leg1, leg2} {
		httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), legID)).Body.Close()
	}

	var finished events.Event
	instA.collector.waitForMatch(t, events.RecordingFinished, func(e events.Event) bool {
		if e.Data.GetRoomID() != rm.ID {
			return false
		}
		finished = e
		return true
	}, 10*time.Second)

	data, ok := finished.Data.(*events.RecordingFinishedData)
	if !ok {
		t.Fatalf("recording.finished carried %T", finished.Data)
	}
	if _, present := data.Channels[play.PlaybackID]; !present {
		t.Fatalf("the leg playback has no channel of its own; channels = %v", data.Channels)
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

// A playback that did not ask to be recorded stays out of the file, which is what
// every caller written before `record` existed gets.
func TestMultiChannel_PlaybackIsNotRecordedUnlessAsked(t *testing.T) {
	instA := newTestInstance(t, "mc-noplay-a")
	instB := newTestInstance(t, "mc-noplay-b")

	leg1, _ := establishCall(t, instA, instB)
	leg2, _ := establishCall(t, instA, instB)

	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{})
	if roomResp.StatusCode != http.StatusCreated {
		t.Fatalf("create room: %d", roomResp.StatusCode)
	}
	var rm roomView
	decodeJSON(t, roomResp, &rm)

	for _, legID := range []string{leg1, leg2} {
		resp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID),
			map[string]interface{}{"leg_id": legID})
		resp.Body.Close()
		id := legID
		instA.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
			return e.Data.GetLegID() == id
		}, 3*time.Second)
	}

	recResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/record", instA.baseURL(), rm.ID),
		map[string]interface{}{"multi_channel": true})
	recResp.Body.Close()
	instA.collector.waitForMatch(t, events.RecordingStarted, func(e events.Event) bool {
		return e.Data.GetRoomID() == rm.ID
	}, 3*time.Second)

	for _, target := range []string{
		fmt.Sprintf("%s/v1/rooms/%s/play", instA.baseURL(), rm.ID),
		fmt.Sprintf("%s/v1/legs/%s/play", instA.baseURL(), leg1),
	} {
		resp := httpPost(t, target, map[string]interface{}{"tone": "dial"})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("play at %s: %d", target, resp.StatusCode)
		}
		resp.Body.Close()
	}
	time.Sleep(300 * time.Millisecond)

	for _, legID := range []string{leg1, leg2} {
		httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), legID)).Body.Close()
	}

	var finished events.Event
	instA.collector.waitForMatch(t, events.RecordingFinished, func(e events.Event) bool {
		if e.Data.GetRoomID() != rm.ID {
			return false
		}
		finished = e
		return true
	}, 10*time.Second)

	data, ok := finished.Data.(*events.RecordingFinishedData)
	if !ok {
		t.Fatalf("recording.finished carried %T", finished.Data)
	}
	if len(data.Channels) != 2 {
		t.Errorf("only the two legs have channels, got %v", data.Channels)
	}
	for id := range data.Channels {
		if strings.HasPrefix(id, "pb-") {
			t.Errorf("a playback nobody asked to record got a channel: %v", data.Channels)
		}
	}
}
