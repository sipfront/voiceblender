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

// A room recording that ends because the room emptied has to announce the multi-channel
// file, not just the mix.
//
// Three paths finalise a room recording — the explicit stop, the room emptying and the room
// being deleted — and they can arrive together. Before cleanupRoomRecording claimed the
// finalisation up front, a second caller took the mix recorder while the owner was still
// merging, published without the multi-channel file, and the owner then found no recorder
// and published nothing. The merged file was on disk and no event named it, so nothing
// downstream knew the per-party audio existed.
func TestMultiChannel_AutoStopAnnouncesTheMergedFile(t *testing.T) {
	instA := newTestInstance(t, "mc-autostop-a")
	instB := newTestInstance(t, "mc-autostop-b")

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

	time.Sleep(250 * time.Millisecond)

	// Empty the room by hanging both legs up, which is what a call ending does. No
	// explicit stop: the auto-stop is the path under test, and both removals racing is
	// what produced the defect.
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
	if data.MultiChannelFile == "" {
		t.Error("the merged multi-channel file has to be announced: it is written either " +
			"way, and an event that omits it leaves the per-party audio invisible")
	}
	if !strings.HasSuffix(data.MultiChannelFile, ".wav") {
		t.Errorf("multi_channel_file = %q", data.MultiChannelFile)
	}
	if len(data.Channels) == 0 {
		t.Error("and the channel map with it, or nothing can tell which track is whose")
	}
	if data.File == "" {
		t.Error("the mix is still announced too")
	}

	// Exactly one recording.finished for this room. Two would mean two finalisations got
	// through, which is the state that produced a half-announced recording.
	matched := instA.collector.matchAll(events.RecordingFinished, func(e events.Event) bool {
		return e.Data.GetRoomID() == rm.ID
	})
	if len(matched) != 1 {
		t.Errorf("recording.finished published %d times for one room, want once", len(matched))
	}
}
