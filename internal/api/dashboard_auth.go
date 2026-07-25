package api

import (
	"crypto/subtle"
	"net/http"
)

// This public status bit lets the static same-origin UI decide whether to show
// its token prompt without probing a protected endpoint.
func (s *Server) handleDashboardAuthStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"auth_required": s.adminToken != ""})
}

// Browser live transports cannot add Authorization. A successful dashboard
// login therefore mints a session-only, HttpOnly cookie accepted only by WS/SSE.
func (s *Server) handleDashboardAuthLogin(w http.ResponseWriter, r *http.Request) {
	if s.adminToken == "" {
		writeJSON(w, http.StatusOK, map[string]bool{"authenticated": true})
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.adminToken)) != 1 {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token", "dashboard login requires Authorization: Bearer <admin_token>")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: dashboardLiveCookie, Value: dashboardLiveCookieValue(s.adminToken), Path: "/api", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": true})
}

func (s *Server) handleDashboardAuthLogout(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: dashboardLiveCookie, Value: "", Path: "/api", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": false})
}
