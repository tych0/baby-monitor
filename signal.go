package main

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"

	"github.com/pion/webrtc/v4"
)

// handleOffer performs single-shot, non-trickle WebRTC signaling: it takes the
// browser's SDP offer, attaches the shared mic track plus a per-client talkback
// track, fully gathers ICE, and returns the answer. A new PeerConnection is
// created per request and closed once it fails, so reconnecting phones don't
// leak connections.
//
// The phone offers three audio m-lines in order: two recvonly (mic + talkback)
// and one sendrecv (its own mic, used only while holding the talk button). We
// AddTrack the mic then the talkback track onto the two recvonly lines; the
// remaining line stays receive-only on our side and surfaces the phone's mic
// via OnTrack, which we relay to the other phones.
func (s *server) handleOffer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var offer webrtc.SessionDescription
	if err := json.Unmarshal(body, &offer); err != nil {
		http.Error(w, "bad offer json", http.StatusBadRequest)
		return
	}

	pc, err := s.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		http.Error(w, "peer connection", http.StatusInternalServerError)
		return
	}

	// Match the offer's m-lines before adding our tracks so AddTrack reuses the
	// phone's recvonly lines in order (mic first, then talkback).
	if err := pc.SetRemoteDescription(offer); err != nil {
		pc.Close()
		http.Error(w, "set remote desc", http.StatusBadRequest)
		return
	}

	talkback, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"talkback", "baby-monitor-talkback",
	)
	if err != nil {
		pc.Close()
		http.Error(w, "create talkback track", http.StatusInternalServerError)
		return
	}

	for _, t := range []webrtc.TrackLocal{s.track, talkback} {
		rtpSender, err := pc.AddTrack(t)
		if err != nil {
			pc.Close()
			http.Error(w, "add track", http.StatusInternalServerError)
			return
		}
		// Drain incoming RTCP (receiver reports, etc.) so interceptors run; this
		// also returns when the connection closes, ending the goroutine.
		go func(sender *webrtc.RTPSender) {
			buf := make([]byte, 1500)
			for {
				if _, _, err := sender.Read(buf); err != nil {
					return
				}
			}
		}(rtpSender)
	}

	// Remember the phone's LAN IP so other phones can name it as the talker; it
	// matches the address /info reports, so every page derives the same name.
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	c := &client{id: id, ip: ip, pc: pc, talkback: &talkbackWriter{track: talkback}}
	s.register(c)

	// The phone's mic arrives here once it starts talking; forward each packet
	// to the other phones (gated by the talk lock inside relayTalk).
	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			pkt, _, err := remote.ReadRTP()
			if err != nil {
				return
			}
			s.relayTalk(c, pkt)
		}
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("peer %s (%s): %s", r.RemoteAddr, id, state)
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			s.unregister(c)
			pc.Close()
		}
	})

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		pc.Close()
		http.Error(w, "create answer", http.StatusInternalServerError)
		return
	}
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		pc.Close()
		http.Error(w, "set local desc", http.StatusInternalServerError)
		return
	}
	<-gatherComplete

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(pc.LocalDescription()); err != nil {
		log.Printf("write answer to %s: %v", r.RemoteAddr, err)
	}
}
