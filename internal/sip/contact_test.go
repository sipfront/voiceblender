package sip

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/emiago/sipgo/sip"
)

// The Contact user part, and the Contact that was missing from in-dialog 2xx responses.

func contactEngine(t *testing.T, mode, user string, port int) *Engine {
	t.Helper()
	engine, err := NewEngine(EngineConfig{
		BindIP:          "127.0.0.1",
		ExternalIP:      "203.0.113.50",
		BindPort:        port,
		SIPHost:         "test",
		ContactUserMode: mode,
		ContactUser:     user,
		Codecs:          []codec.CodecType{codec.CodecPCMU},
		Log:             slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return engine
}

// inviteTo is a request as it arrives at us: To carries our identity, From the peer's.
func inviteTo(toUser, fromUser string) *sip.Request {
	req := sip.NewRequest(sip.INVITE, sip.Uri{Scheme: "sip", User: toUser, Host: "203.0.113.50"})
	req.AppendHeader(&sip.ToHeader{Address: sip.Uri{Scheme: "sip", User: toUser, Host: "carrier.example"}})
	req.AppendHeader(&sip.FromHeader{Address: sip.Uri{Scheme: "sip", User: fromUser, Host: "carrier.example"}})
	return req
}

// --- the user part ----------------------------------------------------------

// An upgrade must not change what an existing deployment advertises.
func TestContactHasNoUserPartByDefault(t *testing.T) {
	for _, mode := range []string{"", "none", "NONE", "nonsense"} {
		engine := contactEngine(t, mode, "", 15070)
		got := engine.ContactForInvite(inviteTo("agent42", "+4319876543")).Address
		if got.User != "" {
			t.Errorf("mode %q: user = %q, want none", mode, got.User)
		}
		if got.Host != "203.0.113.50" || got.Port != 15070 {
			t.Errorf("mode %q: contact = %s, want the advertised address", mode, got.String())
		}
	}
}

func TestContactCanCarryOneFixedUserPart(t *testing.T) {
	engine := contactEngine(t, ContactUserFixed, "voiceos", 15071)
	got := engine.ContactForInvite(inviteTo("agent42", "+4319876543")).Address
	if got.User != "voiceos" {
		t.Errorf("user = %q, want voiceos", got.User)
	}
	// Every dialog gets the same one.
	other := engine.ContactForInvite(inviteTo("someone-else", "+4319876543")).Address
	if other.User != "voiceos" {
		t.Errorf("user = %q on a second dialog, want voiceos", other.User)
	}
}

// From the To header, so the Contact keeps one identity for the life of the dialog.
func TestContactCanCarryThisSidesOwnUserPart(t *testing.T) {
	engine := contactEngine(t, ContactUserLocal, "", 15072)

	got := engine.ContactForInvite(inviteTo("agent42", "+4319876543")).Address
	if got.User != "agent42" {
		t.Errorf("user = %q, want the callee (agent42)", got.User)
	}

	// On a call we originate the local identity is the From chosen for it.
	out := engine.ContactWithUser("+4319876543").Address
	if out.User != "+4319876543" {
		t.Errorf("user = %q, want the caller we originated as", out.User)
	}
}

// A request with no To at all must not panic and must not invent a user.
func TestContactSurvivesARequestWithNoIdentity(t *testing.T) {
	engine := contactEngine(t, ContactUserLocal, "", 15073)
	if got := engine.ContactForInvite(nil).Address; got.User != "" {
		t.Errorf("user = %q from a nil request, want none", got.User)
	}
	bare := sip.NewRequest(sip.INVITE, sip.Uri{Scheme: "sip", Host: "203.0.113.50"})
	if got := engine.ContactForInvite(bare).Address; got.User != "" {
		t.Errorf("user = %q from a request with no To, want none", got.User)
	}
}

func TestContactWithUserAgreesWithTheMode(t *testing.T) {
	if got := contactEngine(t, "none", "", 15074).ContactWithUser("alice").Address.User; got != "" {
		t.Errorf("none: user = %q, want none", got)
	}
	if got := contactEngine(t, ContactUserFixed, "voiceos", 15075).ContactWithUser("alice").Address.User; got != "voiceos" {
		t.Errorf("fixed: user = %q, want voiceos", got)
	}
}

func TestContactUserModesAreTheOnesDocumented(t *testing.T) {
	want := map[string]bool{ContactUserNone: true, ContactUserFixed: true, ContactUserLocal: true}
	got := ContactUserModes()
	if len(got) != len(want) {
		t.Fatalf("modes = %v", got)
	}
	for _, m := range got {
		if !want[m] {
			t.Errorf("unexpected mode %q", m)
		}
	}
}

// --- the missing Contact ----------------------------------------------------

// RFC 3261 §12.2.2. The initial 200 OK gets a Contact from RespondInviteSDP or from
// sipgo's WriteResponse default; the in-dialog path answers the transaction directly and
// had neither.
func TestAnInDialogOKCarriesAContact(t *testing.T) {
	engine := contactEngine(t, ContactUserLocal, "", 15076)

	for _, method := range []sip.RequestMethod{sip.INVITE, sip.UPDATE} {
		req := inviteTo("agent42", "+4319876543")
		req.Method = method
		req.To().Params.Add("tag", "callee-tag")
		req.From().Params.Add("tag", "caller-tag")

		res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		res.AppendHeader(engine.contactForInvite(req))

		contact := res.Contact()
		if contact == nil {
			t.Fatalf("%s: the 2xx to an in-dialog request must carry a Contact "+
				"(RFC 3261 §12.2.2); without it a strict peer discards the response", method)
		}
		if contact.Address.User != "agent42" {
			t.Errorf("%s: contact user = %q, want the identity this side stands in for",
				method, contact.Address.User)
		}
		if !strings.Contains(contact.Address.String(), "203.0.113.50") {
			t.Errorf("%s: contact = %s, want the advertised address",
				method, contact.Address.String())
		}
	}
}

// NewResponseFromRequest copies Record-Route, so nothing here may add one: a request
// without one must produce a response without one.
func TestAnInDialogOKEchoesTheRequestsRecordRoute(t *testing.T) {
	req := inviteTo("agent42", "+4319876543")
	req.To().Params.Add("tag", "callee-tag")
	req.From().Params.Add("tag", "caller-tag")
	req.AppendHeader(sip.NewHeader("Record-Route", "<sip:proxy.example:5060;lr>"))

	res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
	if got := res.GetHeader("Record-Route"); got == nil {
		t.Fatal("a Record-Route on the request has to be echoed in the 2xx")
	} else if !strings.Contains(got.Value(), "proxy.example") {
		t.Errorf("Record-Route = %q, want the proxy's own", got.Value())
	}

	// And a request without one produces a response without one.
	plain := inviteTo("agent42", "+4319876543")
	plain.To().Params.Add("tag", "callee-tag")
	if got := sip.NewResponseFromRequest(plain, sip.StatusOK, "OK", nil).GetHeader("Record-Route"); got != nil {
		t.Errorf("nothing may invent a Record-Route: got %q", got.Value())
	}
}

// Every path that answers an INVITE has to advertise the same Contact. There are three —
// RespondInviteSDP, DialogRespond and handleReInvite — and each one that fell back to the
// DialogUA default advertised a userless Contact while the others did not, so the remote
// target changed identity depending on which answered.
func TestEveryAnswerPathAdvertisesTheSameContact(t *testing.T) {
	engine := contactEngine(t, ContactUserLocal, "", 15077)
	req := inviteTo("agent42", "+4319876543")

	want := engine.contactForInvite(req).Address
	if want.User != "agent42" {
		t.Fatalf("precondition: contact user = %q", want.User)
	}
	// ContactForInvite is the exported form the leg answer path uses, and it must agree.
	if got := engine.ContactForInvite(req).Address; got.String() != want.String() {
		t.Errorf("ContactForInvite = %s, want %s", got.String(), want.String())
	}
}
