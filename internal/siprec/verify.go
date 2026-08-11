package siprec

import (
	"fmt"
	"sort"
	"strings"
)

// MediaSection is one m= section of a recording session's SDP, reduced to the
// fields a metadata document can be checked against.
type MediaSection struct {
	// Label is the a=label value binding this section to a <stream> element.
	Label string
	// CNAME is the a=ssrc cname: value, empty when the offer carries none.
	CNAME string
}

// IssueKind classifies a disagreement between a metadata document and the SDP
// it arrived with.
type IssueKind string

const (
	// IssueDuplicateLabel means two streams claim the same label, so neither
	// resolves to a single participant.
	IssueDuplicateLabel IssueKind = "duplicate_label"
	// IssueUnknownLabel means the metadata labels a stream the SDP never offers.
	IssueUnknownLabel IssueKind = "unknown_label"
	// IssueUnclaimedLabel means the SDP offers a labelled section that no
	// participant sends on, so its audio cannot be attributed to anyone.
	IssueUnclaimedLabel IssueKind = "unclaimed_label"
	// IssueParticipantMismatch means the SDP and the metadata name different
	// parties as the sender of the same section.
	IssueParticipantMismatch IssueKind = "participant_mismatch"
)

// Issue is one metadata/SDP disagreement, identified by the label it concerns.
type Issue struct {
	Kind   IssueKind
	Label  string
	Detail string
}

func (i Issue) String() string {
	if i.Label == "" {
		return fmt.Sprintf("%s: %s", i.Kind, i.Detail)
	}
	return fmt.Sprintf("%s (label %s): %s", i.Kind, i.Label, i.Detail)
}

// Verify cross-checks a metadata document against the SDP it arrived with, and
// returns every disagreement it can prove, ordered deterministically.
//
// The label binding — a=label in the SDP, <label> in the metadata — is the only
// thing that says which recorded stream carries which party. An SRC that gets
// it backwards emits a document that is schema-valid, internally consistent and
// completely wrong: the session establishes, every status code is 200, two
// streams arrive, and every word is attributed to the other participant.
// Nothing else in SIPREC catches that, which is why it is checked here.
//
// Where a section carries an a=ssrc cname naming a SIP URI, that is the SDP's
// own statement about who sends on it, and it must agree with the participant
// the metadata binds to the same label. Only the user part is compared: the
// cname is written by whatever anchored the media and routinely carries a
// different host than the AOR the SRC puts in the metadata.
//
// A nil or empty result means nothing could be disproved — not that the
// document is right. Verify is a guard against silent corruption, not a schema
// validator.
func Verify(r *Recording, sections []MediaSection) []Issue {
	if r == nil {
		return nil
	}

	var issues []Issue

	labelOfStream := make(map[string]string, len(r.Streams))
	seenLabel := make(map[string]string, len(r.Streams))
	duplicated := make(map[string]bool)
	for _, st := range r.Streams {
		if st.StreamID == "" || st.Label == "" {
			continue
		}
		labelOfStream[st.StreamID] = st.Label
		if first, dup := seenLabel[st.Label]; dup {
			if !duplicated[st.Label] {
				duplicated[st.Label] = true
				issues = append(issues, Issue{
					Kind:   IssueDuplicateLabel,
					Label:  st.Label,
					Detail: fmt.Sprintf("streams %s and %s both claim it", first, st.StreamID),
				})
			}
			continue
		}
		seenLabel[st.Label] = st.StreamID
	}

	aorOfParticipant := make(map[string]string, len(r.Participants))
	for i := range r.Participants {
		p := &r.Participants[i]
		if p.ParticipantID != "" {
			aorOfParticipant[p.ParticipantID] = p.AOR()
		}
	}

	senderOfLabel := make(map[string]string, len(r.ParticipantStreams))
	for _, psa := range r.ParticipantStreams {
		if psa.DisassociateTime != "" {
			continue
		}
		for _, streamID := range psa.Send {
			if label, ok := labelOfStream[streamID]; ok {
				senderOfLabel[label] = psa.ParticipantID
			}
		}
	}

	offered := make(map[string]MediaSection, len(sections))
	for _, sec := range sections {
		if sec.Label != "" {
			offered[sec.Label] = sec
		}
	}

	for label := range seenLabel {
		if _, ok := offered[label]; !ok {
			issues = append(issues, Issue{
				Kind:   IssueUnknownLabel,
				Label:  label,
				Detail: "no m= section in the offer carries this label",
			})
		}
	}

	// Only meaningful once the document labels anything at all; a document with
	// no labelled streams is a different (and already visible) problem.
	if len(seenLabel) > 0 {
		for label := range offered {
			if _, ok := senderOfLabel[label]; !ok {
				issues = append(issues, Issue{
					Kind:   IssueUnclaimedLabel,
					Label:  label,
					Detail: "no participant sends on this section",
				})
			}
		}
	}

	for label, sec := range offered {
		// A duplicated label binds to no single participant, which the
		// duplicate_label issue already says.
		if duplicated[label] {
			continue
		}
		cnameUser := aorUser(sec.CNAME)
		if cnameUser == "" {
			continue
		}
		participantID, ok := senderOfLabel[label]
		if !ok {
			continue
		}
		metaUser := aorUser(aorOfParticipant[participantID])
		if metaUser == "" || strings.EqualFold(metaUser, cnameUser) {
			continue
		}
		issues = append(issues, Issue{
			Kind:  IssueParticipantMismatch,
			Label: label,
			Detail: fmt.Sprintf("offer says %s sends on it, metadata assigns it to %s (%s)",
				cnameUser, metaUser, participantID),
		})
	}

	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Label != issues[j].Label {
			return issues[i].Label < issues[j].Label
		}
		return issues[i].Kind < issues[j].Kind
	})
	return issues
}

// aorUser returns the user part of a SIP AOR or an RTCP cname shaped like one.
// A value with no user part yields "", which callers treat as "no claim made".
func aorUser(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if i := strings.IndexByte(v, ':'); i >= 0 {
		switch strings.ToLower(v[:i]) {
		case "sip", "sips", "tel":
			v = v[i+1:]
		}
	}
	if i := strings.IndexByte(v, '@'); i >= 0 {
		return v[:i]
	}
	return ""
}
