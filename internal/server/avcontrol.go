package server

import (
	"errors"
	"net/http"

	"streaming/internal/avcontrol"
)

func (s *Server) handleAVPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if len(s.pages.AV) == 0 {
		http.NotFound(w, r)
		return
	}
	writeHTML(w, s.pages.AV)
}

func (s *Server) handleAVStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if s.av == nil {
		writeJSON(w, http.StatusOK, avcontrol.Status{
			Phase:  "off",
			Status: "AV control is not configured",
		})
		return
	}
	writeJSON(w, http.StatusOK, s.av.Status())
}

func (s *Server) handleAVOn(w http.ResponseWriter, r *http.Request) {
	s.postAV(w, r, func() error { return s.av.TurnOn() })
}

func (s *Server) handleAVOff(w http.ResponseWriter, r *http.Request) {
	s.postAV(w, r, func() error { return s.av.TurnOff() })
}

func (s *Server) handleAVDelay(w http.ResponseWriter, r *http.Request) {
	s.postAV(w, r, func() error { return s.av.DelayOff() })
}

func (s *Server) postAV(w http.ResponseWriter, r *http.Request, action func() error) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.av == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "AV control is not configured"})
		return
	}
	if err := action(); err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, avcontrol.ErrNotConfigured) {
			code = http.StatusServiceUnavailable
		}
		if errors.Is(err, avcontrol.ErrNoOff) || errors.Is(err, avcontrol.ErrButtonLocked) {
			code = http.StatusConflict
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.av.Status())
}
