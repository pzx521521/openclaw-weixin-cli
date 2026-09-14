package cloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	ilink "github.com/pzx521521/openclaw-weixin-cli/internal/ilink"
)

// Server wires HTTP handlers to the store.
type Server struct {
	store *Store
	poll  time.Duration
	web   string
}

// NewServer builds the HTTP server dependencies.
func NewServer(store *Store, pollTimeout time.Duration, webDir string) *Server {
	if pollTimeout <= 0 {
		pollTimeout = 2 * time.Second
	}
	return &Server{store: store, poll: pollTimeout, web: webDir}
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

// authBotID resolves the login session cookie to a bot_id.
func (s *Server) authBotID(r *http.Request) (string, bool) {
	token, ok := cookieToken(r)
	if !ok {
		return "", false
	}
	botID, err := s.store.GetSessionUser(r.Context(), token)
	if err != nil {
		return "", false
	}
	return botID, true
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
		if status.ILinkBotID == "" || status.BotToken == "" {
			writeErr(w, http.StatusBadGateway, "login confirmed but token or bot id missing")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"status":    "confirmed",
			"bot_id":    status.ILinkBotID,
			"user_id":   status.ILinkUserID,
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
	BotID    string `json:"bot_id"`
	UserID   string `json:"user_id"`
	BaseURL  string `json:"base_url"`
}

// handleRegisterFinish verifies the scanned token and creates the account.
func (s *Server) handleRegisterFinish(w http.ResponseWriter, r *http.Request) {
	var req finishReq
	if !decodeBody(w, r, &req) {
		return
	}
	req.BotID = strings.TrimSpace(req.BotID)
	req.BotToken = strings.TrimSpace(req.BotToken)
	if len(req.Password) < minPassLen {
		writeErr(w, http.StatusBadRequest, "password too short, at least 6 characters")
		return
	}
	if req.BotID == "" || req.BotToken == "" {
		writeErr(w, http.StatusBadRequest, "missing bot_id or bot_token, scan the QR code first")
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
	if err := s.store.CreateUser(r.Context(), req.BotID, hash, req.BotToken, strings.TrimSpace(req.UserID), baseURL); err != nil {
		if errors.Is(err, ErrUserExists) {
			writeErr(w, http.StatusConflict, "bot_id already registered, please login")
			return
		}
		writeErr(w, http.StatusInternalServerError, "create user: "+err.Error())
		return
	}

	token, err := s.store.CreateSession(r.Context(), req.BotID, sessionTTL)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "create session: "+err.Error())
		return
	}
	setSessionCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]string{"bot_id": req.BotID})
}

type loginReq struct {
	BotID    string `json:"bot_id"`
	Password string `json:"password"`
}

// handleLogin verifies bot_id + password and issues a session cookie.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if !decodeBody(w, r, &req) {
		return
	}
	req.BotID = strings.TrimSpace(req.BotID)
	u, err := s.store.GetUser(r.Context(), req.BotID)
	if err != nil || CheckPassword(u.PasswordHash, req.Password) != nil {
		writeErr(w, http.StatusUnauthorized, "bot_id 或密码错误")
		return
	}
	// Note: err==nil here implies u != nil; CheckPassword(nil) would fail above.
	token, err := s.store.CreateSession(r.Context(), u.BotID, sessionTTL)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "create session: "+err.Error())
		return
	}
	setSessionCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]string{"bot_id": u.BotID})
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
	botID, ok := s.authBotID(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	u, err := s.store.GetUser(r.Context(), botID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load user: "+err.Error())
		return
	}
	peer := ""
	if to, _, err := s.resolveTargetFromUser(u); err == nil {
		peer = to
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"bot_id": botID,
		"ready":  u.Ready,
		"peer":   peer,
	})
}

// handleReady polls once and reports whether the first inbound arrived.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	botID, ok := s.authBotID(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.poll+10*time.Second)
	defer cancel()

	if _, err := s.shortPoll(ctx, botID, s.poll); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	to, _, err := s.resolveTarget(ctx, botID)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ready": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ready": true, "peer": to})
}

type sendReq struct {
	Text     string `json:"text"`
	BotID    string `json:"bot_id"`
	Password string `json:"password"`
}

// handleSend supports cookie sessions and direct bot_id + password auth.
func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	var req sendReq
	if !decodeBody(w, r, &req) {
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		writeErr(w, http.StatusBadRequest, "missing text")
		return
	}

	botID, ok := s.authBotID(r)
	if !ok {
		// Direct API auth: bot_id + password in body.
		req.BotID = strings.TrimSpace(req.BotID)
		if req.BotID == "" || req.Password == "" {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		u, err := s.store.GetUser(r.Context(), req.BotID)
		if err != nil || CheckPassword(u.PasswordHash, req.Password) != nil {
			writeErr(w, http.StatusUnauthorized, "bot_id 或密码错误")
			return
		}
		botID = u.BotID
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.poll+20*time.Second)
	defer cancel()

	to, msgs, sent, sendErr := s.sendText(ctx, botID, text, s.poll)
	out := map[string]any{"to": to, "msgs": msgs, "sent": sent}
	if !sent {
		out["error"] = sendErr
	}
	writeJSON(w, http.StatusOK, out)
}
