package main

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// TestTalkbackReachesOtherPhone is the end-to-end talkback path: phone A acquires
// the mic and sends RTP; phone B must receive it on its talkback track. This is
// the regression test for talkback being dropped, and it exercises the full
// ReadRTP -> rewrite -> WriteRTP relay.
func TestTalkbackReachesOtherPhone(t *testing.T) {
	_, ts := newTalkTestServer(t)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Phone B receives; count packets that arrive on the talkback track.
	phoneB, _ := phoneLikeBrowser(t)
	defer phoneB.Close()
	var talkbackPkts int64
	phoneB.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			if _, _, err := track.ReadRTP(); err != nil {
				return
			}
			if track.ID() == "talkback" {
				atomic.AddInt64(&talkbackPkts, 1)
			}
		}
	})
	connectPhone(t, ts, "B", phoneB)

	// Phone A transmits.
	phoneA, micSender := phoneLikeBrowser(t)
	defer phoneA.Close()
	connectPhone(t, ts, "A", phoneA)

	resp, err := http.Post(ts.URL+"/talk?id=A&op=acquire", "", nil)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("acquire status = %d", resp.StatusCode)
	}

	micTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"mic", "phoneA-mic")
	if err != nil {
		t.Fatalf("mic track: %v", err)
	}
	if err := micSender.ReplaceTrack(micTrack); err != nil {
		t.Fatalf("replace track: %v", err)
	}

	go func() {
		pkt := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SSRC: 0x12345678}, Payload: []byte{0xF8, 0xFF, 0xFE}}
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				pkt.SequenceNumber++
				pkt.Timestamp += opusFrameTS
				_ = micTrack.WriteRTP(pkt)
			}
		}
	}()

	deadline := time.After(20 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("phone B never received talkback (got %d pkts)", atomic.LoadInt64(&talkbackPkts))
		case <-time.After(200 * time.Millisecond):
			if atomic.LoadInt64(&talkbackPkts) > 0 {
				return
			}
		}
	}
}

// phoneLikeBrowser builds a peer with the page's three audio m-lines and returns
// it plus the sender for its own mic (the third, sendrecv line).
func phoneLikeBrowser(t *testing.T) (*webrtc.PeerConnection, *webrtc.RTPSender) {
	t.Helper()
	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(defaultMediaEngine(t)),
		webrtc.WithSettingEngine(loopbackSettingEngine()),
	)
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("phone pc: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio,
			webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			t.Fatalf("recv transceiver %d: %v", i, err)
		}
	}
	tx, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv})
	if err != nil {
		t.Fatalf("send transceiver: %v", err)
	}
	return pc, tx.Sender()
}

// connectPhone runs the non-trickle signaling handshake against /offer.
func connectPhone(t *testing.T, ts *httptest.Server, id string, pc *webrtc.PeerConnection) {
	t.Helper()
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local: %v", err)
	}
	<-gather
	answer := postOffer(t, ts.URL+"/offer?id="+id, pc.LocalDescription())
	if err := pc.SetRemoteDescription(answer); err != nil {
		t.Fatalf("set remote: %v", err)
	}
}

// TestTalkbackWriterRewrite checks that the per-subscriber rewriter presents one
// continuous stream: sequence numbers step by one and timestamps advance by the
// source's delta, and across a talker switch they stay continuous instead of
// jumping to the new source's unrelated clock.
func TestTalkbackWriterRewrite(t *testing.T) {
	w := &talkbackWriter{}

	pkt := func(seq uint16, ts uint32) *rtp.Packet {
		return &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts}}
	}

	// First packet from talker A anchors the stream.
	o := w.rewrite("A", pkt(1000, 5000))
	if o.SequenceNumber != 1000 || o.Timestamp != 5000 {
		t.Fatalf("first packet = seq %d ts %d, want 1000/5000", o.SequenceNumber, o.Timestamp)
	}

	// Two more from A, 20ms apart: seq +1 each, ts follows A's delta.
	o = w.rewrite("A", pkt(1001, 5960))
	if o.SequenceNumber != 1001 || o.Timestamp != 5960 {
		t.Fatalf("A pkt2 = seq %d ts %d, want 1001/5960", o.SequenceNumber, o.Timestamp)
	}
	o = w.rewrite("A", pkt(1002, 6920))
	if o.SequenceNumber != 1002 || o.Timestamp != 6920 {
		t.Fatalf("A pkt3 = seq %d ts %d, want 1002/6920", o.SequenceNumber, o.Timestamp)
	}

	// Talker switches to B, whose clock is unrelated (huge seq/ts). The output
	// must remain continuous: seq +1, ts +one frame — never B's raw values.
	o = w.rewrite("B", pkt(40000, 999999))
	if o.SequenceNumber != 1003 {
		t.Fatalf("after switch seq = %d, want 1003 (continuous)", o.SequenceNumber)
	}
	if o.Timestamp != 6920+opusFrameTS {
		t.Fatalf("after switch ts = %d, want %d (continuous)", o.Timestamp, 6920+opusFrameTS)
	}

	// Subsequent B packets advance by B's own delta off the continued base.
	prevTS := o.Timestamp
	o = w.rewrite("B", pkt(40001, 999999+960))
	if o.SequenceNumber != 1004 || o.Timestamp != prevTS+960 {
		t.Fatalf("B pkt2 = seq %d ts %d, want 1004/%d", o.SequenceNumber, o.Timestamp, prevTS+960)
	}
}

// TestTalkerEventsFeed verifies the /events SSE stream reports the current talker
// and every change (acquire/release), so receivers can show who holds the mic.
func TestTalkerEventsFeed(t *testing.T) {
	srv, ts := newTalkTestServer(t)
	defer ts.Close()

	// A connected client is required before it can acquire the lock; register one
	// directly (no media needed for this test).
	srv.register(&client{id: "a"})

	// Open the SSE stream and read data: lines as they arrive.
	resp, err := http.Get(ts.URL + "/events")
	if err != nil {
		t.Fatalf("open events: %v", err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "data:") {
				lines <- strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
	}()

	next := func(want string) {
		t.Helper()
		select {
		case got := <-lines:
			if got != want {
				t.Fatalf("event = %q, want %q", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for event %q", want)
		}
	}

	next("") // initial state: nobody talking

	if code := talkOp(t, ts, "a", "acquire"); code != http.StatusNoContent {
		t.Fatalf("acquire = %d", code)
	}
	next("a")

	if code := talkOp(t, ts, "a", "release"); code != http.StatusNoContent {
		t.Fatalf("release = %d", code)
	}
	next("")
}
