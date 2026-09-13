package api

import (
	"net/http"

	"github.com/Dhaval0607/wallet-p2p/web"
)

func servePage(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

func (s *Server) handleLogViewer(w http.ResponseWriter, r *http.Request) {
	servePage(w, web.LogsHTML)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	servePage(w, web.DashboardHTML)
}
