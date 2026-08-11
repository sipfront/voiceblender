package siprec

import (
	"fmt"
	"testing"
)

// metadataXML renders the shape a session-recording client emits for a
// two-party call: participant A on stream s1, participant B on s2, with each
// stream's label supplied by the caller so a test can invert the binding.
func metadataXML(aorA, labelA, aorB, labelB string) []byte {
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<recording xmlns="urn:ietf:params:xml:ns:recording:1">
  <datamode>complete</datamode>
  <session session_id="sess-1"/>
  <participant participant_id="pA" session_id="sess-1"><nameID aor="%s"/></participant>
  <participant participant_id="pB" session_id="sess-1"><nameID aor="%s"/></participant>
  <stream stream_id="s1" session_id="sess-1"><label>%s</label></stream>
  <stream stream_id="s2" session_id="sess-1"><label>%s</label></stream>
  <participantstreamassoc participant_id="pA"><send>s1</send><recv>s2</recv></participantstreamassoc>
  <participantstreamassoc participant_id="pB"><send>s2</send><recv>s1</recv></participantstreamassoc>
</recording>`, aorA, aorB, labelA, labelB))
}

// twoPartyOffer is the shape a session recording client sends: one labelled
// section per party, each naming its sender in a=ssrc cname. The cname hosts
// differ from the AOR domain, as they do when a media relay writes them.
func twoPartyOffer() []MediaSection {
	return []MediaSection{
		{Label: "0", CNAME: "sip:alice@10.0.0.1"},
		{Label: "1", CNAME: "sip:bob@10.0.0.2"},
	}
}

func TestVerify(t *testing.T) {
	const aorA = "sip:alice@example.com"
	const aorB = "sip:bob@example.com"

	cases := []struct {
		name     string
		metadata []byte
		sections []MediaSection
		want     []Issue
	}{
		{
			name:     "labels agree with the offer",
			metadata: metadataXML(aorA, "0", aorB, "1"),
			sections: twoPartyOffer(),
		},
		{
			// Valid and self-consistent, but the offer says otherwise.
			name:     "caller and callee inverted",
			metadata: metadataXML(aorA, "1", aorB, "0"),
			sections: twoPartyOffer(),
			want: []Issue{
				{Kind: IssueParticipantMismatch, Label: "0",
					Detail: "offer says alice sends on it, metadata assigns it to bob (pB)"},
				{Kind: IssueParticipantMismatch, Label: "1",
					Detail: "offer says bob sends on it, metadata assigns it to alice (pA)"},
			},
		},
		{
			// The cname host is written by whatever anchored the media and does
			// not have to match the AOR domain.
			name:     "cname host differs from the AOR domain",
			metadata: metadataXML(aorA, "0", aorB, "1"),
			sections: []MediaSection{
				{Label: "0", CNAME: "sip:alice@10.0.0.1"},
				{Label: "1", CNAME: "sip:bob@10.0.0.2"},
			},
		},
		{
			name:     "offer carries no cname",
			metadata: metadataXML(aorA, "1", aorB, "0"),
			sections: []MediaSection{{Label: "0"}, {Label: "1"}},
		},
		{
			name:     "cname is not a URI",
			metadata: metadataXML(aorA, "1", aorB, "0"),
			sections: []MediaSection{{Label: "0", CNAME: "randomcname"}, {Label: "1", CNAME: "other"}},
		},
		{
			name:     "both streams claim one label",
			metadata: metadataXML(aorA, "0", aorB, "0"),
			sections: []MediaSection{{Label: "0", CNAME: aorA}},
			want: []Issue{
				{Kind: IssueDuplicateLabel, Label: "0", Detail: "streams s1 and s2 both claim it"},
			},
		},
		{
			name:     "metadata labels a stream the offer does not carry",
			metadata: metadataXML(aorA, "0", aorB, "9"),
			sections: twoPartyOffer(),
			want: []Issue{
				{Kind: IssueUnclaimedLabel, Label: "1", Detail: "no participant sends on this section"},
				{Kind: IssueUnknownLabel, Label: "9", Detail: "no m= section in the offer carries this label"},
			},
		},
		{
			// An offer with no labels is no evidence about any label. Reporting
			// every stream as unknown would be noise, not a finding.
			name:     "offer carries no labels at all",
			metadata: metadataXML(aorA, "0", aorB, "1"),
			sections: []MediaSection{{CNAME: aorA}, {CNAME: aorB}},
		},
		{
			name:     "no offer at all",
			metadata: metadataXML(aorA, "0", aorB, "1"),
			sections: nil,
		},
		{
			// tel URIs have no host, so the comparison must not fall back to
			// "no claim made" and silently skip the check.
			name:     "tel URIs, inverted",
			metadata: metadataXML("tel:+43111", "1", "tel:+43222", "0"),
			sections: []MediaSection{
				{Label: "0", CNAME: "tel:+43111"},
				{Label: "1", CNAME: "tel:+43222"},
			},
			want: []Issue{
				{Kind: IssueParticipantMismatch, Label: "0",
					Detail: "offer says +43111 sends on it, metadata assigns it to +43222 (pB)"},
				{Kind: IssueParticipantMismatch, Label: "1",
					Detail: "offer says +43222 sends on it, metadata assigns it to +43111 (pA)"},
			},
		},
		{
			name:     "tel URIs, agreeing",
			metadata: metadataXML("tel:+43111", "0", "tel:+43222", "1"),
			sections: []MediaSection{
				{Label: "0", CNAME: "tel:+43111;phone-context=+43"},
				{Label: "1", CNAME: "tel:+43222"},
			},
		},
		{
			name:     "offer carries a section nobody sends on",
			metadata: metadataXML(aorA, "0", aorB, "1"),
			sections: append(twoPartyOffer(), MediaSection{Label: "2", CNAME: "sip:carol@example.com"}),
			want: []Issue{
				{Kind: IssueUnclaimedLabel, Label: "2", Detail: "no participant sends on this section"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, err := Parse(tc.metadata)
			if err != nil {
				t.Fatalf("parse metadata: %v", err)
			}
			got := Verify(rec, tc.sections)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d issues, want %d\n got: %v\nwant: %v", len(got), len(tc.want), got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("issue %d:\n got: %+v\nwant: %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// A participant that has left is no longer the sender, so its section must be
// reported as unclaimed rather than silently keeping the old attribution.
func TestVerifyIgnoresDisassociatedSender(t *testing.T) {
	md := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<recording xmlns="urn:ietf:params:xml:ns:recording:1">
  <participant participant_id="pA"><nameID aor="sip:alice@example.com"/></participant>
  <stream stream_id="s1"><label>0</label></stream>
  <participantstreamassoc participant_id="pA"><send>s1</send><disassociate-time>2026-08-10T10:00:00Z</disassociate-time></participantstreamassoc>
</recording>`)

	rec, err := Parse(md)
	if err != nil {
		t.Fatalf("parse metadata: %v", err)
	}

	got := Verify(rec, []MediaSection{{Label: "0", CNAME: "sip:alice@example.com"}})
	want := []Issue{{Kind: IssueUnclaimedLabel, Label: "0", Detail: "no participant sends on this section"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestVerifyNilRecording(t *testing.T) {
	if got := Verify(nil, twoPartyOffer()); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

func TestAORUser(t *testing.T) {
	cases := map[string]string{
		"sip:alice@example.com":    "alice",
		"sips:alice@example.com":   "alice",
		"SIP:Alice@example.com":    "Alice",
		"tel:+4312345@example.com": "+4312345",
		"alice@example.com":        "alice",
		"randomcname":              "",
		"":                         "",
		"   ":                      "",
		"sip:bob@1.2.3.4":          "bob",

		// A tel URI carries no host (RFC 3966).
		"tel:+4312345":                   "+4312345",
		"TEL:+4312345;phone-context=+43": "+4312345",
		// A sip URI without a user part names a host, which identifies nobody.
		"sip:example.com": "",
	}
	for in, want := range cases {
		if got := aorUser(in); got != want {
			t.Errorf("aorUser(%q) = %q, want %q", in, got, want)
		}
	}
}
