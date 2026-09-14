package cloud

import "net/http"

// Mount registers API routes and the static fallback.
func (s *Server) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /api/register/qr", s.handleRegisterQR)
	mux.HandleFunc("GET /api/register/status", s.handleRegisterStatus)
	mux.HandleFunc("POST /api/register/finish", s.handleRegisterFinish)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("GET /api/account/ready", s.handleReady)
	mux.HandleFunc("POST /api/send", s.handleSend)
	mux.HandleFunc("OPTIONS /api/send", s.handleSend)
	mux.Handle("/", s.staticHandler())
}
