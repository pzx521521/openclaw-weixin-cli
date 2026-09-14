package cloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	ilink "github.com/pzx521521/openclaw-weixin-cli/internal/ilink"
)

// Server wires HTTP handlers to the store.
type Server struct {
	store *Store
	// poll bounds the ready long wait; sendPoll bounds the send history fetch.
	// sendPoll == 0 means pure cache send (no GetUpdates call).
	poll     time.Duration
	sendPoll time.Duration
	web      string
}

// NewServer builds the HTTP server dependencies.
func NewServer(store *Store, pollTimeout, sendTimeout time.Duration, webDir string) *Server {
	if pollTimeout <= 0 {
		pollTimeout = 60 * time.Second
	}
	if sendTimeout < 0 {
		sendTimeout = DefaultSendTimeout
	}
	return &Server{store: store, poll: pollTimeout, sendPoll: sendTimeout, web: webDir}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body")
		return false
	}
	return true
}

// authUserID resolves the login session cookie to a user_id.
func (s *Server) authUserID(r *http.Request) (string, bool) {
	token, ok := cookieToken(r)
	if !ok {
		return "", false
	}
	userID, err := s.store.GetSessionUser(r.Context(), token)
	if err != nil {
		return "", false
	}
	return userID, true
}

// handleHealth is an unauthenticated liveness check (DB pinged).
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type qrReq struct {
	Password string `json:"password"`
}

// handleRegisterQR starts a QR login and returns the code + PNG (stateless).
func (s *Server) handleRegisterQR(w http.ResponseWriter, r *http.Request) {
	var req qrReq
	if !decodeBody(w, r, &req) {
		return
	}
	if len(req.Password) < minPassLen {
		writeErr(w, http.StatusBadRequest, "password too short, at least 6 characters")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	client := ilink.NewClient(ilink.DefaultBaseURL, "")
	qrResp, err := client.FetchLoginQRCode(ctx, ilink.DefaultBotType)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "fetch qrcode: "+err.Error())
		return
	}
	png, err := qrcode.Encode(qrResp.QRCodeImgContent, qrcode.Medium, 384)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "render qrcode: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"qrcode":     qrResp.QRCode,
		"qr_png":     base64.StdEncoding.EncodeToString(png),
		"expires_in": 480,
	})
}

// handleRegisterStatus proxies the QR login status (stateless).
func (s *Server) handleRegisterStatus(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(r.URL.Query().Get("qrcode"))
	if code == "" {
		writeErr(w, http.StatusBadRequest, "missing qrcode")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	client := ilink.NewClient(ilink.DefaultBaseURL, "")
	status, err := client.PollLoginStatus(ctx, code)
	if err != nil {
		if ilink.IsTimeoutError(err) {
			writeJSON(w, http.StatusOK, map[string]string{"status": "wait"})
			return
		}
		writeErr(w, http.StatusBadGateway, "poll status: "+err.Error())
		return
	}

	switch status.Status {
	case "confirmed":
		if strings.TrimSpace(status.ILinkUserID) == "" || status.BotToken == "" {
			writeErr(w, http.StatusBadGateway, "login confirmed but token or user id missing, please rescan")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"status":    "confirmed",
			"user_id":   strings.TrimSpace(status.ILinkUserID),
			"base_url":  ilink.NormalizeBaseURL(status.BaseURL),
			"bot_token": status.BotToken,
		})
	case "expired":
		writeJSON(w, http.StatusOK, map[string]string{"status": "expired"})
	case "scaned", "wait":
		writeJSON(w, http.StatusOK, map[string]string{"status": status.Status})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": status.Status})
	}
}

type finishReq struct {
	Password string `json:"password"`
	BotToken string `json:"bot_token"`
	UserID   string `json:"user_id"`
	BaseURL  string `json:"base_url"`
}

// handleRegisterFinish verifies the scanned token and creates the account.
func (s *Server) handleRegisterFinish(w http.ResponseWriter, r *http.Request) {
	var req finishReq
	if !decodeBody(w, r, &req) {
		return
	}
	req.UserID = strings.TrimSpace(req.UserID)
	req.BotToken = strings.TrimSpace(req.BotToken)
	if len(req.Password) < minPassLen {
		writeErr(w, http.StatusBadRequest, "password too short, at least 6 characters")
		return
	}
	if req.UserID == "" || req.BotToken == "" {
		writeErr(w, http.StatusBadRequest, "missing user_id or bot_token, scan the QR code first")
		return
	}
	baseURL := ilink.NormalizeBaseURL(req.BaseURL)

	// Verify the token works before storing it.
	ctx, cancel := context.WithTimeout(r.Context(), verifyTimeout)
	defer cancel()
	check := ilink.NewClient(baseURL, req.BotToken)
	resp, err := check.GetUpdates(ctx, "", ilink.ChannelVersion, 10*time.Second)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "token verify failed: "+err.Error())
		return
	}
	if resp.Ret != 0 || resp.ErrCode != 0 {
		writeErr(w, http.StatusBadRequest, "token verify failed: server rejected the token")
		return
	}

	hash, err := HashPassword(req.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "hash password: "+err.Error())
		return
	}
	overwritten, err := s.store.UpsertUserBinding(r.Context(), req.UserID, hash, req.BotToken, baseURL)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "save user: "+err.Error())
		return
	}

	token, err := s.store.CreateSession(r.Context(), req.UserID, sessionTTL)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "create session: "+err.Error())
		return
	}
	setSessionCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":     req.UserID,
		"overwritten": overwritten,
	})
}

type loginReq struct {
	UserID   string `json:"user_id"`
	Password string `json:"password"`
}

// handleLogin verifies user_id + password and issues a session cookie.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if !decodeBody(w, r, &req) {
		return
	}
	u, err := s.store.GetUser(r.Context(), req.UserID)
	if err != nil || CheckPassword(u.PasswordHash, req.Password) != nil {
		writeErr(w, http.StatusUnauthorized, "账号或密码错误")
		return
	}
	// Note: err==nil here implies u != nil; CheckPassword(nil) would fail above.
	// Re-login refreshes the token: drop all previous sessions first.
	_ = s.store.DeleteUserSessions(r.Context(), u.UserID)
	token, err := s.store.CreateSession(r.Context(), u.UserID, sessionTTL)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "create session: "+err.Error())
		return
	}
	setSessionCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]string{"user_id": u.UserID})
}

// handleLogout drops the session.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token, ok := cookieToken(r); ok {
		_ = s.store.DeleteSession(r.Context(), token)
	}
	clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleMe returns the current account summary.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.authUserID(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	u, err := s.store.GetUser(r.Context(), userID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load user: "+err.Error())
		return
	}
	peer := ""
	if to, _, err := s.resolveTargetFromUser(u); err == nil {
		peer = to
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":      userID,
		"ready":        u.Ready,
		"peer":         peer,
		"poll_timeout": int(s.poll.Seconds()),
	})
}

// handleReady waits up to the poll timeout for the first inbound message.
// A single getupdates may return early-empty, so loop until the deadline.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.authUserID(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.poll+10*time.Second)
	defer cancel()

	deadline := time.Now().Add(s.poll)
	for {
		if time.Until(deadline) <= 0 {
			writeJSON(w, http.StatusOK, map[string]any{"ready": false})
			return
		}
		if _, err := s.shortPoll(ctx, userID, time.Until(deadline)); err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		if to, _, err := s.resolveTarget(ctx, userID); err == nil {
			writeJSON(w, http.StatusOK, map[string]any{"ready": true, "peer": to})
			return
		}
	}
}

// setCORSHeaders allows cross-origin direct calls (user_id + password, no cookies).
func setCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Max-Age", "86400")
}

type sendReq struct {
	Text     string `json:"text"`
	UserID   string `json:"user_id"`
	Password string `json:"password"`
}

// handleSend supports cookie sessions and direct user_id + password auth.
func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	setCORSHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var req sendReq
	if !decodeBody(w, r, &req) {
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		writeErr(w, http.StatusBadRequest, "missing text")
		return
	}

	userID, ok := s.authUserID(r)
	if !ok {
		// Direct API auth: user_id + password in body.
		req.UserID = strings.TrimSpace(req.UserID)
		if req.UserID == "" || req.Password == "" {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		u, err := s.store.GetUser(r.Context(), req.UserID)
		if err != nil || CheckPassword(u.PasswordHash, req.Password) != nil {
			writeErr(w, http.StatusUnauthorized, "账号或密码错误")
			return
		}
		userID = u.UserID
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.sendPoll+20*time.Second)
	defer cancel()

	to, msgs, sent, sendErr := s.sendText(ctx, userID, text, s.sendPoll)
	out := map[string]any{"to": to, "msgs": msgs, "sent": sent}
	if !sent {
		out["error"] = sendErr
	}
	writeJSON(w, http.StatusOK, out)
}
