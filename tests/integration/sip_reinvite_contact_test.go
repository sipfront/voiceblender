//go:build integration

package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/emiago/sipgo/sip"
)

// sendInDialogInvite sends a re-INVITE from this client, acting as the UAS of the dialog
// that `inv` established, and returns the final response.
func (c *rawSIPClient) sendInDialogInvite(t *testing.T, inv *sip.Request, okRes *sip.Response, sdp []byte) *sip.Response {
	t.Helper()

	peerContact := inv.Contact()
	if peerContact == nil {
		t.Fatal("INVITE missing Contact header")
	}
	callID, from, to := inv.CallID(), inv.From(), okRes.To()
	if callID == nil || from == nil || to == nil {
		t.Fatal("dialog headers missing")
	}

	req := sip.NewRequest(sip.INVITE, peerContact.Address)
	req.AppendHeader(&sip.FromHeader{Address: to.Address, Params: to.Params.Clone()})
	req.AppendHeader(&sip.ToHeader{Address: from.Address, Params: from.Params.Clone()})
	req.AppendHeader(sip.HeaderClone(callID))
	req.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Scheme: "sip", Host: c.host, Port: c.port}})
	if len(sdp) > 0 {
		req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		req.SetBody(sdp)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := c.client.Do(ctx, req)
	if err != nil {
		t.Fatalf("re-INVITE Do: %v", err)
	}
	return resp
}

// establishedCall dials this client from the instance and answers, returning the INVITE
// and the 200 OK so an in-dialog request can be built from the dialog they made.
func establishedCall(t *testing.T, inst *testInstance, cli *rawSIPClient) (*sip.Request, *sip.Response) {
	t.Helper()
	cli.sendRegister(t, inst.sipPort, "alice", cli.contactURI("alice"), 600)

	resp := httpPost(t, inst.baseURL()+"/v1/legs", map[string]interface{}{
		"type": "sip", "to": "sip:alice@vb.test", "from": "support",
		"codecs": []string{"PCMU"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: %d", resp.StatusCode)
	}
	resp.Body.Close()

	e := cli.waitInvite(t, 5*time.Second)
	okRes := cli.answerInviteCaptured(t, e)
	// Let the UAC dialog finish establishing so MatchRequestDialog can find it.
	time.Sleep(200 * time.Millisecond)
	return e.req, okRes
}

// A 2xx to a re-INVITE must carry a Contact (RFC 3261 §12.2.2). Without it a strict peer
// discards the response, retransmits the re-INVITE to timer B and tears down a call that
// was up and carrying audio.
func TestSIPReInvite_OKCarriesContact(t *testing.T) {
	inst := newTestInstance(t, "reinvite-contact")
	cli := newRawSIPClient(t, "reinvite-contact-ua")
	inv, okRes := establishedCall(t, inst, cli)

	resp := cli.sendInDialogInvite(t, inv, okRes, nil)
	if resp.StatusCode != sip.StatusOK {
		t.Fatalf("re-INVITE status = %d %s, want 200", resp.StatusCode, resp.Reason)
	}
	contact := resp.Contact()
	if contact == nil {
		t.Fatal("200 OK to a re-INVITE has no Contact")
	}
	if contact.Address.Host == "" {
		t.Errorf("Contact = %s, want a routable address", contact.Address.String())
	}
}

// A Record-Route on the re-INVITE has to come back on the response, and one that was not
// asked for must not be invented: the established route set is the peer's to keep.
func TestSIPReInvite_OKEchoesRecordRouteOnlyWhenAsked(t *testing.T) {
	inst := newTestInstance(t, "reinvite-rr")
	cli := newRawSIPClient(t, "reinvite-rr-ua")
	inv, okRes := establishedCall(t, inst, cli)

	if rr := cli.sendInDialogInvite(t, inv, okRes, nil).GetHeader("Record-Route"); rr != nil {
		t.Errorf("Record-Route = %q on a re-INVITE that carried none", rr.Value())
	}
}

// The Contact user part, over the wire, in the mode a back-to-back user agent uses. The
// leg is dialled with from=support, so support is the local identity of that dialog.
func TestSIPContactUser_LocalMode(t *testing.T) {
	inst := newTestInstanceWithOpts(t, "contact-local", func(c *config.Config) {
		c.SIPContactUserMode = sipmod.ContactUserLocal
	})
	cli := newRawSIPClient(t, "contact-local-ua")
	cli.sendRegister(t, inst.sipPort, "alice", cli.contactURI("alice"), 600)

	resp := httpPost(t, inst.baseURL()+"/v1/legs", map[string]interface{}{
		"type": "sip", "to": "sip:alice@vb.test", "from": "support",
		"codecs": []string{"PCMU"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: %d", resp.StatusCode)
	}
	resp.Body.Close()

	e := cli.waitInvite(t, 5*time.Second)
	contact := e.req.Contact()
	if contact == nil {
		t.Fatal("outbound INVITE has no Contact")
	}
	if contact.Address.User != "support" {
		t.Errorf("Contact user = %q, want support", contact.Address.User)
	}

	// The same identity on the 2xx to a re-INVITE. That is the property worth pinning:
	// the peer's in-dialog request carries our identity in To, so reading it there keeps
	// one Contact identity for the life of the dialog rather than flipping to the peer's.
	okRes := cli.answerInviteCaptured(t, e)
	time.Sleep(200 * time.Millisecond)
	reContact := cli.sendInDialogInvite(t, e.req, okRes, nil).Contact()
	if reContact == nil {
		t.Fatal("200 OK to a re-INVITE has no Contact")
	}
	if reContact.Address.User != contact.Address.User {
		t.Errorf("Contact user changed mid-dialog: %q on the INVITE, %q on the re-INVITE 2xx",
			contact.Address.User, reContact.Address.User)
	}
}

// The default is unchanged: no user part.
func TestSIPContactUser_NoneByDefault(t *testing.T) {
	inst := newTestInstance(t, "contact-none")
	cli := newRawSIPClient(t, "contact-none-ua")
	cli.sendRegister(t, inst.sipPort, "alice", cli.contactURI("alice"), 600)

	resp := httpPost(t, inst.baseURL()+"/v1/legs", map[string]interface{}{
		"type": "sip", "to": "sip:alice@vb.test", "from": "support",
		"codecs": []string{"PCMU"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: %d", resp.StatusCode)
	}
	resp.Body.Close()

	contact := cli.waitInvite(t, 5*time.Second).req.Contact()
	if contact == nil {
		t.Fatal("outbound INVITE has no Contact")
	}
	if contact.Address.User != "" {
		t.Errorf("Contact user = %q, want none by default", contact.Address.User)
	}
}
