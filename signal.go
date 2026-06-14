package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"

	"github.com/pion/webrtc/v4"
)

// handleOffer performs single-shot, non-trickle WebRTC signaling: it takes the
// browser's SDP offer, attaches the shared mic track, fully gathers ICE, and
// returns the answer. A new PeerConnection is created per request and closed
// once it fails, so reconnecting phones don't leak connections.
func (s *server) handleOffer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
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

	rtpSender, err := pc.AddTrack(s.track)
	if err != nil {
		pc.Close()
		http.Error(w, "add track", http.StatusInternalServerError)
		return
	}
	// Drain incoming RTCP (receiver reports, etc.) so interceptors run; this
	// also returns when the connection closes, ending the goroutine.
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := rtpSender.Read(buf); err != nil {
				return
			}
		}
	}()

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("peer %s: %s", r.RemoteAddr, state)
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			pc.Close()
		}
	})

	if err := pc.SetRemoteDescription(offer); err != nil {
		pc.Close()
		http.Error(w, "set remote desc", http.StatusBadRequest)
		return
	}
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
