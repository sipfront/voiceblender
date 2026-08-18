//go:build integration

package integration

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// A room listener hears the room and contributes nothing to it.
//
// /v1/rooms/{id}/ws joins the mixer as an ordinary participant, so before it was muted on
// join whatever a client sent was mixed into the room and heard by everyone in it. For a
// room that bridges a live call that is audio injected into somebody's conversation by a
// socket that only meant to listen — and the console's own words for the feature are "a
// one-way relay of the room mix: nothing is ever sent into the call".
func TestRoomWS_ListenerCannotBeHeard(t *testing.T) {
	inst := newTestInstance(t, "room-ws-mute")

	createResp := httpPost(t, inst.baseURL()+"/v1/rooms", map[string]any{
		"id": "mute-room", "sample_rate": 16000,
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create room: %d", createResp.StatusCode)
	}
	createResp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	// A leg in the room, which is who would hear a listener that was not muted.
	legURL := "ws://" + inst.httpAddr +
		"/v1/legs/websocket?sample_rate=16000&wire_format=json_base64&room_id=mute-room"
	legConn, err := wsDial(ctx, legURL)
	if err != nil {
		t.Fatalf("dial leg WS: %v", err)
	}
	defer legConn.Close()

	ringing := inst.collector.waitForMatch(t, events.LegRinging, nil, 3*time.Second)
	legID := ringing.Data.GetLegID()
	inst.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, 3*time.Second)

	// Two listeners: one talks, the other is the ear. Two rather than one because a
	// participant never hears itself, so a single listener could not tell "muted" from
	// "mixed-minus-self".
	talker, err := wsDial(ctx, "ws://"+inst.httpAddr+"/v1/rooms/mute-room/ws")
	if err != nil {
		t.Fatalf("dial talking listener: %v", err)
	}
	defer talker.Close()
	ear, err := wsDial(ctx, "ws://"+inst.httpAddr+"/v1/rooms/mute-room/ws")
	if err != nil {
		t.Fatalf("dial listening listener: %v", err)
	}
	defer ear.Close()

	// The leg sends nothing, so anything the ear hears above the noise floor came from
	// the talker.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		var phase float64
		const dPhase = 2 * math.Pi * 1000.0 / 16000.0
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				pcm := make([]byte, 640)
				for i := 0; i < 320; i++ {
					binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(12000*math.Sin(phase))))
					phase += dPhase
				}
				frame, _ := json.Marshal(map[string]string{
					"audio": base64.StdEncoding.EncodeToString(pcm),
				})
				if err := wsutil.WriteClientText(talker, frame); err != nil {
					return
				}
			}
		}
	}()

	// A short window on purpose: this suite's audio tests are timing-sensitive and every
	// second another test holds sockets and a 20 ms ticker open is a second they can fail
	// in. A tone this loud is decisive within a few frames.
	deadline := time.Now().Add(800 * time.Millisecond)
	var peak int16
	for time.Now().Before(deadline) {
		wsutilx.SetReadDeadline(ear, 500*time.Millisecond)
		hdr, err := ws.ReadHeader(ear)
		if err != nil {
			break
		}
		payload := make([]byte, hdr.Length)
		if _, err := io.ReadFull(ear, payload); err != nil {
			break
		}
		if hdr.Masked {
			ws.Cipher(payload, hdr.Mask, 0)
		}
		var frame struct {
			Type  string `json:"type"`
			Audio string `json:"audio"`
		}
		if json.Unmarshal(payload, &frame) != nil || frame.Audio == "" || frame.Type == "ping" {
			continue
		}
		pcm, err := base64.StdEncoding.DecodeString(frame.Audio)
		if err != nil {
			continue
		}
		for i := 0; i+1 < len(pcm); i += 2 {
			s := int16(binary.LittleEndian.Uint16(pcm[i:]))
			if s < 0 {
				s = -s
			}
			if s > peak {
				peak = s
			}
		}
		if peak > 2000 {
			break // decided; nothing is learned by listening longer
		}
	}

	// Comfort noise is small; a 12000-amplitude tone is not. 2000 separates them with room
	// to spare in both directions.
	if peak > 2000 {
		t.Errorf("a room listener was audible to the room: peak %d — whatever a listening "+
			"socket sends must never reach the call", peak)
	}
}
