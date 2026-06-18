package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"
)

// newTalkTestServer builds a server (via the same newServer used by main) behind
// an httptest server exposing /offer and /talk.
func newTalkTestServer(t *testing.T) (*server, *httptest.Server) {
	t.Helper()
	srv, err := newServer(loopbackSettingEngine())
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/offer", srv.handleOffer)
	mux.HandleFunc("/talk", srv.handleTalk)
	return srv, httptest.NewServer(mux)
}

// offerLikePhone reproduces the page's m-line layout: two recvonly lines (baby
// mic, talkback) and a sendrecv line for the phone's own mic.
func offerLikePhone(t *testing.T) *webrtc.PeerConnection {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("phone pc: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio,
			webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			t.Fatalf("recv transceiver %d: %v", i, err)
		}
	}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv}); err != nil {
		t.Fatalf("send transceiver: %v", err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local: %v", err)
	}
	return pc
}

// TestAnswerDirections verifies the core wiring: the server sends on the two
// recvonly lines (baby mic + talkback) and receives on the third (phone mic).
// If this ordering breaks, talkback audio is routed to the wrong m-line.
func TestAnswerDirections(t *testing.T) {
	srv, ts := newTalkTestServer(t)
	defer ts.Close()

	phone := offerLikePhone(t)
	defer phone.Close()
	answer := postOffer(t, ts.URL+"/offer?id=p1", phone.LocalDescription())

	dirs := mediaDirections(answer.SDP)
	if len(dirs) != 3 {
		t.Fatalf("want 3 audio m-lines, got %d: %v", len(dirs), dirs)
	}
	// Server sends baby mic + talkback, then receives the phone's mic.
	want := []string{"sendonly", "sendonly", "recvonly"}
	for i, w := range want {
		if dirs[i] != w {
			t.Errorf("m-line %d direction = %q, want %q (all: %v)", i, dirs[i], w, dirs)
		}
	}
	if _, ok := srv.clients["p1"]; !ok {
		t.Errorf("client p1 not registered after offer")
	}
}

// TestTalkLock verifies only one phone holds the mic at a time and that the lock
// is reusable after release.
func TestTalkLock(t *testing.T) {
	srv, ts := newTalkTestServer(t)
	defer ts.Close()

	a := offerLikePhone(t)
	defer a.Close()
	postOffer(t, ts.URL+"/offer?id=a", a.LocalDescription())
	b := offerLikePhone(t)
	defer b.Close()
	postOffer(t, ts.URL+"/offer?id=b", b.LocalDescription())

	if code := talkOp(t, ts, "a", "acquire"); code != http.StatusNoContent {
		t.Fatalf("a acquire = %d, want 204", code)
	}
	if code := talkOp(t, ts, "b", "acquire"); code != http.StatusConflict {
		t.Fatalf("b acquire while a holds = %d, want 409", code)
	}
	if code := talkOp(t, ts, "a", "release"); code != http.StatusNoContent {
		t.Fatalf("a release = %d, want 204", code)
	}
	if code := talkOp(t, ts, "b", "acquire"); code != http.StatusNoContent {
		t.Fatalf("b acquire after release = %d, want 204", code)
	}
	srv.mu.Lock()
	talker := srv.talker
	srv.mu.Unlock()
	if talker != "b" {
		t.Errorf("talker = %q, want b", talker)
	}
}

func talkOp(t *testing.T, ts *httptest.Server, id, op string) int {
	t.Helper()
	resp, err := http.Post(ts.URL+"/talk?id="+id+"&op="+op, "", nil)
	if err != nil {
		t.Fatalf("talk %s %s: %v", id, op, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// mediaDirections returns the a=send/recv direction of each audio m-section, in
// order.
func mediaDirections(sdp string) []string {
	var dirs []string
	inAudio := false
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "m=") {
			inAudio = strings.HasPrefix(line, "m=audio")
			if inAudio {
				dirs = append(dirs, "") // filled when we hit the direction attr
			}
			continue
		}
		if !inAudio {
			continue
		}
		switch line {
		case "a=sendrecv", "a=sendonly", "a=recvonly", "a=inactive":
			dirs[len(dirs)-1] = strings.TrimPrefix(line, "a=")
		}
	}
	return dirs
}
