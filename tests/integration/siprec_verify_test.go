//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/siprec"
)

// mislabelledMetadata binds participant B to a label the offer never carries.
// The document is valid and self-consistent; only the SDP contradicts it.
func mislabelledMetadata(t *testing.T) []byte {
	t.Helper()
	rec := &siprec.Recording{
		DataMode:               siprec.DataModeComplete,
		SessionRecordingAssocs: []siprec.SessionRecordingAssoc{{SessionID: "s1"}},
		Participants: []siprec.Participant{
			{ParticipantID: "pa", NameIDs: []siprec.NameID{{AOR: "sip:alice@example.com"}}},
			{ParticipantID: "pb", NameIDs: []siprec.NameID{{AOR: "sip:bob@example.com"}}},
		},
		Streams: []siprec.Stream{
			{StreamID: "ta", Label: "1"},
			{StreamID: "tb", Label: "9"},
		},
		ParticipantStreams: []siprec.ParticipantStreamAssoc{
			{ParticipantID: "pa", Send: []string{"ta"}},
			{ParticipantID: "pb", Send: []string{"tb"}},
		},
	}
	md, err := rec.Marshal()
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	return md
}

func siprecLegID(t *testing.T, srs *testInstance) string {
	t.Helper()
	evt := srs.collector.waitForMatch(t, events.SIPRECSessionStarted, nil, 5*time.Second)
	getter, _ := evt.Data.(interface{ GetLegID() string })
	if getter == nil || getter.GetLegID() == "" {
		t.Fatal("siprec.session_started carries no leg ID")
	}
	return getter.GetLegID()
}

func TestSIPREC_MetadataAgreeingWithOfferIsNotFlagged(t *testing.T) {
	src := newTestInstance(t, "src")
	srs := siprecInstance(t, "srs", nil)

	call, err := dialSIPREC(t, src, srs, twoPartyMetadata(t))
	if err != nil {
		t.Fatalf("SIPREC INVITE failed: %v", err)
	}
	defer call.Dialog.Bye(context.Background())

	view := getSIPRECSession(t, srs, siprecLegID(t, srs))
	if len(view.Warnings) != 0 {
		t.Fatalf("metadata matching the offer was flagged: %v", view.Warnings)
	}
}

// A document that contradicts its own SDP is still answered and recorded, but
// the disagreement has to be visible.
func TestSIPREC_MetadataDisagreeingWithOfferIsFlagged(t *testing.T) {
	src := newTestInstance(t, "src")
	srs := siprecInstance(t, "srs", nil)

	call, err := dialSIPREC(t, src, srs, mislabelledMetadata(t))
	if err != nil {
		t.Fatalf("SIPREC INVITE failed: %v", err)
	}
	defer call.Dialog.Bye(context.Background())

	if call.RemoteSDP == nil || len(call.RemoteSDP.Audio) != 2 {
		t.Fatal("the session must still be answered on both streams")
	}

	view := getSIPRECSession(t, srs, siprecLegID(t, srs))
	if len(view.Warnings) != 2 {
		t.Fatalf("got %d warnings, want 2: %v", len(view.Warnings), view.Warnings)
	}

	joined := strings.Join(view.Warnings, "\n")
	for _, want := range []string{
		string(siprec.IssueUnknownLabel) + " (label 9)",
		string(siprec.IssueUnclaimedLabel) + " (label 2)",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings do not mention %q:\n%s", want, joined)
		}
	}

	// Flagged, not rejected: the participants are still bound and exposed.
	if len(view.Participants) != 2 {
		t.Errorf("participants = %d, want 2 — a flagged session is still recorded", len(view.Participants))
	}
}
