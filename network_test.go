package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// TestIsVirtualIface checks that virtual/container/VPN interfaces are skipped
// while real LAN hardware (and the loopback names that don't match a prefix)
// is kept, so lanIP only ever advertises a reachable address.
func TestIsVirtualIface(t *testing.T) {
	cases := []struct {
		name    string
		virtual bool
	}{
		{"eth0", false},
		{"wlan0", false},
		{"enp3s0", false},
		{"eno1", false},
		{"docker0", true},
		{"br-1a2b3c", true},
		{"veth8f0c1a", true},
		{"tailscale0", true},
		{"tun0", true},
		{"tap0", true},
		{"virbr0", true},
		{"vmnet1", true},
		{"vboxnet0", true},
		{"zt0", true},
		{"wg0", true},
	}
	for _, c := range cases {
		if got := isVirtualIface(c.name); got != c.virtual {
			t.Errorf("isVirtualIface(%q) = %v, want %v", c.name, got, c.virtual)
		}
	}
}

// TestLanIP makes sure that when lanIP finds an address it is a usable private
// IPv4 one (never loopback, never a non-private/CGNAT address). On CI there may
// be no private interface at all, in which case the empty result is acceptable.
func TestLanIP(t *testing.T) {
	ip := lanIP()
	if ip == "" {
		t.Skip("no private LAN IPv4 address on this host")
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		t.Fatalf("lanIP() = %q, not a valid IP", ip)
	}
	if parsed.To4() == nil {
		t.Errorf("lanIP() = %q, want an IPv4 address", ip)
	}
	if parsed.IsLoopback() {
		t.Errorf("lanIP() = %q, must not be loopback", ip)
	}
	if !parsed.IsPrivate() {
		t.Errorf("lanIP() = %q, want a private address", ip)
	}
}

// TestHandleIndex verifies the embedded client page is served at "/" and that
// every other path 404s rather than falling through.
func TestHandleIndex(t *testing.T) {
	srv := &server{}

	rec := httptest.NewRecorder()
	srv.handleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("GET / Content-Type = %q, want text/html", ct)
	}
	if rec.Body.Len() == 0 {
		t.Error("GET / returned an empty body")
	}

	rec = httptest.NewRecorder()
	srv.handleIndex(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /nope = %d, want 404", rec.Code)
	}
}

// TestHandleOfferRejectsBadRequests covers the signaling endpoint's input
// validation: only POST is allowed and the body must be valid offer JSON.
func TestHandleOfferRejectsBadRequests(t *testing.T) {
	srv, err := newServer(loopbackSettingEngine())
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.handleOffer(rec, httptest.NewRequest(http.MethodGet, "/offer", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /offer = %d, want 405", rec.Code)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/offer?id=x", bytes.NewBufferString("not json"))
	srv.handleOffer(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST /offer with garbage body = %d, want 400", rec.Code)
	}

	// An offer without a client id can't be tied to the talk lock, so it's rejected.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/offer", bytes.NewBufferString("{}"))
	srv.handleOffer(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST /offer without id = %d, want 400", rec.Code)
	}
}

// TestSignalingForwardsMediaToSubscriber is the end-to-end networking test: it
// runs the real /offer signaling handler and UDP RTP ingest, connects a
// receive-only "phone" peer over loopback ICE, pushes Opus RTP packets into the
// UDP feed, and asserts they are fanned out across the shared track to the
// subscriber. This exercises the whole capture->server->phone media path.
func TestSignalingForwardsMediaToSubscriber(t *testing.T) {
	srv, err := newServer(loopbackSettingEngine())
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/offer", srv.handleOffer)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start the UDP RTP reader on a free port and forward into the shared track.
	rtpPort := freeUDPPort(t)
	go readRTP(ctx, rtpPort, srv.track)

	// Build the receive-only client peer (the phone).
	clientAPI := webrtc.NewAPI(
		webrtc.WithMediaEngine(defaultMediaEngine(t)),
		webrtc.WithSettingEngine(loopbackSettingEngine()),
	)
	pc, err := clientAPI.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("client peer connection: %v", err)
	}
	defer pc.Close()

	// Mirror the browser's three audio m-lines: two recvonly (baby mic +
	// talkback) and one sendrecv (this phone's mic). The server fans the shared
	// mic track out onto the first recvonly line.
	for i := 0; i < 2; i++ {
		if _, err := pc.AddTransceiverFromKind(
			webrtc.RTPCodecTypeAudio,
			webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly},
		); err != nil {
			t.Fatalf("add recv transceiver %d: %v", i, err)
		}
	}
	if _, err := pc.AddTransceiverFromKind(
		webrtc.RTPCodecTypeAudio,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv},
	); err != nil {
		t.Fatalf("add send transceiver: %v", err)
	}

	gotRTP := make(chan struct{}, 1)
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if _, _, err := track.ReadRTP(); err != nil {
			return
		}
		select {
		case gotRTP <- struct{}{}:
		default:
		}
	})

	// Single-shot, non-trickle signaling, mirroring the browser client.
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local description: %v", err)
	}
	<-gather

	answer := postOffer(t, ts.URL+"/offer?id=phone", pc.LocalDescription())
	if err := pc.SetRemoteDescription(answer); err != nil {
		t.Fatalf("set remote description: %v", err)
	}

	// Push Opus RTP into the UDP feed until the subscriber sees a packet; the
	// first datagrams race DTLS/SRTP setup, so resend on a steady tick.
	go pumpRTP(ctx, t, rtpPort)

	select {
	case <-gotRTP:
		// Media made it across the WebRTC connection.
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for RTP to reach the subscriber")
	}
}

// loopbackSettingEngine configures ICE to use only loopback host candidates so
// the WebRTC handshake completes deterministically inside CI (no real LAN, no
// mDNS, no external STUN).
func loopbackSettingEngine() webrtc.SettingEngine {
	se := webrtc.SettingEngine{}
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetInterfaceFilter(func(name string) bool {
		iface, err := net.InterfaceByName(name)
		return err == nil && iface.Flags&net.FlagLoopback != 0
	})
	return se
}

// defaultMediaEngine returns a MediaEngine with the standard codecs registered,
// matching what newServer uses for the server side.
func defaultMediaEngine(t *testing.T) *webrtc.MediaEngine {
	t.Helper()
	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		t.Fatalf("register codecs: %v", err)
	}
	return me
}

// freeUDPPort asks the OS for an unused UDP port on loopback and returns it.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("reserve udp port: %v", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	conn.Close()
	return port
}

// postOffer sends an SDP offer to the signaling endpoint and decodes the answer.
func postOffer(t *testing.T, url string, offer *webrtc.SessionDescription) webrtc.SessionDescription {
	t.Helper()
	body, err := json.Marshal(offer)
	if err != nil {
		t.Fatalf("marshal offer: %v", err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post offer: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post offer status = %d, want 200", resp.StatusCode)
	}
	var answer webrtc.SessionDescription
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	return answer
}

// pumpRTP repeatedly sends a minimal Opus RTP packet to the UDP feed until ctx
// is cancelled, so delivery doesn't depend on hitting the exact moment SRTP
// becomes ready.
func pumpRTP(ctx context.Context, t *testing.T, rtpPort int) {
	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: rtpPort})
	if err != nil {
		t.Errorf("dial udp feed: %v", err)
		return
	}
	defer conn.Close()

	pkt := &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 111, SSRC: 0xDEADBEEF},
		Payload: []byte{0xF8, 0xFF, 0xFE}, // a tiny but valid Opus frame
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pkt.SequenceNumber++
			pkt.Timestamp += 960 // 20ms at 48kHz
			raw, err := pkt.Marshal()
			if err != nil {
				t.Errorf("marshal rtp: %v", err)
				return
			}
			if _, err := conn.Write(raw); err != nil {
				return
			}
		}
	}
}
