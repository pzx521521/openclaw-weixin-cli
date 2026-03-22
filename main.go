package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

const (
	defaultStatePath = "session.json"
	defaultQRPath    = "login-qr.png"
	channelVersion   = "1.0.2"
)

var (
	logger      = newLogger(os.Stdout)
	errorLogger = newLogger(os.Stderr)
)

// ChatRegistry tracks the latest reply context for each peer.
type ChatRegistry struct {
	mu      sync.RWMutex
	current string
	peers   map[string]SessionPeer
}

// NewChatRegistry builds the in-memory chat state from persisted session data.
func NewChatRegistry(state *SessionState) *ChatRegistry {
	peers := make(map[string]SessionPeer)
	current := ""
	if state != nil {
		for peer, saved := range state.Peers {
			peer = strings.TrimSpace(peer)
			saved.ContextToken = strings.TrimSpace(saved.ContextToken)
			saved.LastSeenAt = strings.TrimSpace(saved.LastSeenAt)
			if peer == "" || saved.ContextToken == "" {
				continue
			}
			peers[peer] = saved
		}
		current = strings.TrimSpace(state.CurrentPeer)
		if current != "" {
			if _, ok := peers[current]; !ok {
				current = ""
			}
		}
	}
	return &ChatRegistry{
		current: current,
		peers:   peers,
	}
}

// Upsert records the latest context token for one peer.
func (r *ChatRegistry) Upsert(peer, contextToken string, seenAt time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if peer == "" {
		return
	}

	entry := r.peers[peer]
	if contextToken != "" {
		entry.ContextToken = contextToken
	}
	if !seenAt.IsZero() {
		entry.LastSeenAt = seenAt.Format(time.RFC3339)
	}
	r.peers[peer] = entry
	if r.current == "" {
		r.current = peer
	}
}

// SetCurrent switches the active peer used by plain text input.
func (r *ChatRegistry) SetCurrent(peer string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.peers[peer]; !ok {
		return fmt.Errorf("unknown peer: %s", peer)
	}
	r.current = peer
	return nil
}

// Current returns the selected peer and its context token.
func (r *ChatRegistry) Current() (string, string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.current == "" {
		return "", "", false
	}
	peer, ok := r.peers[r.current]
	return r.current, peer.ContextToken, ok
}

// List returns the known peers in stable order.
func (r *ChatRegistry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	peers := make([]string, 0, len(r.peers))
	for peer := range r.peers {
		peers = append(peers, peer)
	}
	sort.Slice(peers, func(i, j int) bool {
		left := r.peers[peers[i]].LastSeenAt
		right := r.peers[peers[j]].LastSeenAt
		if left == right {
			return peers[i] < peers[j]
		}
		return left > right
	})
	return peers
}

// Token returns the latest context token for one peer.
func (r *ChatRegistry) Token(peer string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, ok := r.peers[peer]
	return entry.ContextToken, ok
}

// LastSeenAt returns the last inbound time recorded for one peer.
func (r *ChatRegistry) LastSeenAt(peer string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.peers[peer].LastSeenAt
}

// ApplyToState copies the current registry snapshot into the persisted session.
func (r *ChatRegistry) ApplyToState(state *SessionState) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	state.CurrentPeer = r.current
	state.Peers = make(map[string]SessionPeer, len(r.peers))
	for peer, saved := range r.peers {
		state.Peers[peer] = saved
	}
}

// main runs the CLI entrypoint.
func main() {
	if err := run(os.Args[1:]); err != nil {
		errorLogger.Error("程序退出", "error", err)
		os.Exit(1)
	}
}

// run dispatches explicit subcommands or falls back to auto mode.
func run(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "login":
			return runLogin(args[1:])
		case "chat":
			return runChat(args[1:])
		case "help", "-h", "--help":
			logUsage()
			return nil
		}
	}
	return runAuto(args)
}

// runAuto chooses chat when an existing session is usable, otherwise login.
func runAuto(args []string) error {
	fs := flag.NewFlagSet("auto", flag.ContinueOnError)
	statePath := fs.String("state", defaultStatePath, "path to session state JSON")
	baseURL := fs.String("base-url", defaultBaseURL, "iLink API base URL")
	qrPath := fs.String("qr", defaultQRPath, "where to save the QR PNG")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if HasUsableSession(*statePath) {
		logger.Info("检测到可用 session，进入聊天模式", "state", *statePath)
		return runChat([]string{"-state", *statePath})
	}

	logger.Info("未检测到可用 session，进入登录模式", "state", *statePath)
	return runLogin([]string{"-state", *statePath, "-base-url", *baseURL, "-qr", *qrPath})
}

// runLogin performs QR login and persists the received token.
func runLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	statePath := fs.String("state", defaultStatePath, "path to session state JSON")
	baseURL := fs.String("base-url", defaultBaseURL, "iLink API base URL")
	qrPath := fs.String("qr", defaultQRPath, "where to save the QR PNG")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := signalContext()
	defer cancel()

	client := NewClient(normalizeBaseURL(*baseURL), "")
	qrResp, err := client.FetchLoginQRCode(ctx, defaultBotType)
	if err != nil {
		return err
	}

	qrFile, err := saveQRCodeImage(*qrPath, qrResp.QRCodeImgContent)
	if err != nil {
		return err
	}

	logger.Info("二维码已生成，请用微信扫码确认", "qr", qrFile)

	status, err := waitForLogin(ctx, client, qrResp.QRCode)
	if err != nil {
		return err
	}

	state := &SessionState{
		BotToken: status.BotToken,
		BotID:    status.ILinkBotID,
		UserID:   status.ILinkUserID,
		BaseURL:  normalizeBaseURL(status.BaseURL),
	}
	if err := SaveState(*statePath, state); err != nil {
		return err
	}

	logger.Info("登录成功", "bot_id", state.BotID, "user_id", state.UserID)
	logger.Info("session 已保存", "state", *statePath)
	logger.Info("正在进入聊天模式", "state", *statePath)
	return runChat([]string{"-state", *statePath})
}

// waitForLogin blocks until the QR login is confirmed or expires.
func waitForLogin(ctx context.Context, client *Client, qrcode string) (*QRStatusResponse, error) {
	deadline := time.Now().Add(8 * time.Minute)

	for time.Now().Before(deadline) {
		pollCtx, cancel := context.WithTimeout(ctx, defaultLongPollTimeout)
		status, err := client.PollLoginStatus(pollCtx, qrcode)
		cancel()
		if err != nil {
			if isTimeoutError(err) {
				continue
			}
			return nil, err
		}

		switch status.Status {
		case "wait":
		case "scaned":
			logger.Info("二维码已扫码，请在手机上确认登录")
		case "confirmed":
			if status.ILinkBotID == "" || status.BotToken == "" {
				return nil, errors.New("login confirmed but token or bot id missing")
			}
			if strings.TrimSpace(status.BaseURL) == "" {
				status.BaseURL = defaultBaseURL
			}
			return status, nil
		case "expired":
			return nil, errors.New("QR code expired, rerun login")
		default:
			logger.Info("登录状态更新", "status", status.Status)
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}

	return nil, errors.New("login timed out")
}

// runChat starts long-polling and a terminal reply loop.
func runChat(args []string) error {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	statePath := fs.String("state", defaultStatePath, "path to session state JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	state, err := LoadState(*statePath)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	if state.BotToken == "" {
		return errors.New("missing bot token, run login first")
	}

	ctx, cancel := signalContext()
	defer cancel()

	client := NewClient(state.BaseURL, state.BotToken)
	registry := NewChatRegistry(state)
	var persistMu sync.Mutex

	logger.Info("聊天模式已启动", "bot_id", state.BotID)
	if peers := registry.List(); len(peers) > 0 {
		logger.Info("已从 session 恢复用户", "count", len(peers))
	}
	logChatHelp()

	var ioMu sync.Mutex
	var once sync.Once
	stopWithErr := func(err error) {
		if err == nil {
			return
		}
		if errors.Is(err, context.Canceled) {
			cancel()
			return
		}
		once.Do(func() {
			ioMu.Lock()
			errorLogger.Error("聊天模式退出", "error", err)
			ioMu.Unlock()
			cancel()
		})
	}

	go func() {
		stopWithErr(pollLoop(ctx, client, state, *statePath, registry, &ioMu, &persistMu))
	}()

	if err := inputLoop(ctx, client, state, *statePath, registry, &ioMu, &persistMu); err != nil {
		stopWithErr(err)
	}

	<-ctx.Done()
	return nil
}

// pollLoop keeps fetching inbound messages and updating the saved cursor.
func pollLoop(ctx context.Context, client *Client, state *SessionState, statePath string, registry *ChatRegistry, ioMu *sync.Mutex, persistMu *sync.Mutex) error {
	timeout := defaultLongPollTimeout

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		resp, err := client.GetUpdates(ctx, state.GetUpdatesBuf, channelVersion, timeout)
		if err != nil {
			return err
		}

		if resp.LongPollingTimeoutMS > 0 {
			timeout = time.Duration(resp.LongPollingTimeoutMS) * time.Millisecond
		}

		if resp.ErrCode != 0 || resp.Ret != 0 {
			return fmt.Errorf("getupdates ret=%d errcode=%d errmsg=%s", resp.Ret, resp.ErrCode, resp.ErrMsg)
		}

		if resp.GetUpdatesBuf != "" && resp.GetUpdatesBuf != state.GetUpdatesBuf {
			state.GetUpdatesBuf = resp.GetUpdatesBuf
			if err := persistState(statePath, state, registry, persistMu); err != nil {
				return err
			}
		}

		for _, msg := range resp.Msgs {
			if err := handleInbound(msg, state, statePath, registry, ioMu, persistMu); err != nil {
				return err
			}
		}
	}
}

// handleInbound logs one inbound message and caches its reply token.
func handleInbound(msg WeixinMessage, state *SessionState, statePath string, registry *ChatRegistry, ioMu *sync.Mutex, persistMu *sync.Mutex) error {
	from := strings.TrimSpace(msg.FromUserID)
	if from == "" {
		return nil
	}

	beforeToken, beforeOK := registry.Token(from)
	beforeCurrent, _, _ := registry.Current()
	seenAt := time.Now()
	if msg.CreateTimeMS > 0 {
		seenAt = time.UnixMilli(msg.CreateTimeMS)
	}
	registry.Upsert(from, strings.TrimSpace(msg.ContextToken), seenAt)
	afterToken, _ := registry.Token(from)
	afterCurrent, _, _ := registry.Current()
	text := extractMessageText(msg)
	if text == "" {
		text = summarizeMessage(msg)
	}

	if !beforeOK || beforeToken != afterToken || beforeCurrent != afterCurrent {
		if err := persistState(statePath, state, registry, persistMu); err != nil {
			return err
		}
	}

	ioMu.Lock()
	logger.Info("收到消息", "from", from, "text", text)
	printPromptLocked(registry)
	ioMu.Unlock()
	return nil
}

// inputLoop reads terminal commands and sends outbound replies.
func inputLoop(ctx context.Context, client *Client, state *SessionState, statePath string, registry *ChatRegistry, ioMu *sync.Mutex, persistMu *sync.Mutex) error {
	scanner := bufio.NewScanner(os.Stdin)

	ioMu.Lock()
	printPromptLocked(registry)
	ioMu.Unlock()

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			ioMu.Lock()
			printPromptLocked(registry)
			ioMu.Unlock()
			continue
		}

		if strings.HasPrefix(line, "/") {
			if err := handleCommand(ctx, line, client, state, statePath, registry, ioMu, persistMu); err != nil {
				if errors.Is(err, context.Canceled) {
					return err
				}
				errorLogger.Error("命令执行失败", "error", err)
			}
		} else {
			if err := sendToCurrent(ctx, client, registry, line); err != nil {
				ioMu.Lock()
				errorLogger.Error("发送失败", "error", err)
				ioMu.Unlock()
			}
		}

		ioMu.Lock()
		printPromptLocked(registry)
		ioMu.Unlock()
	}

	if err := scanner.Err(); err != nil {
		return err
	}
	return ctx.Err()
}

// handleCommand executes one terminal command.
func handleCommand(ctx context.Context, line string, client *Client, state *SessionState, statePath string, registry *ChatRegistry, ioMu *sync.Mutex, persistMu *sync.Mutex) error {
	fields := strings.Fields(line)
	switch fields[0] {
	case "/help":
		ioMu.Lock()
		logChatHelp()
		ioMu.Unlock()
	case "/users":
		ioMu.Lock()
		peers := registry.List()
		if len(peers) == 0 {
			logger.Info("当前还没有活跃用户")
		} else {
			logger.Info("已知用户列表", "count", len(peers))
			for _, peer := range peers {
				logger.Info("用户", "peer", peer, "last_seen_at", registry.LastSeenAt(peer))
			}
		}
		ioMu.Unlock()
	case "/who":
		ioMu.Lock()
		if peer, _, ok := registry.Current(); ok {
			logger.Info("当前用户", "peer", peer)
		} else {
			logger.Info("当前还没有选中用户")
		}
		ioMu.Unlock()
	case "/use":
		if len(fields) < 2 {
			return errors.New("usage: /use <peer>")
		}
		if err := registry.SetCurrent(fields[1]); err != nil {
			return err
		}
		if err := persistState(statePath, state, registry, persistMu); err != nil {
			return err
		}
		ioMu.Lock()
		logger.Info("已切换当前用户", "peer", fields[1])
		ioMu.Unlock()
	case "/send":
		if len(fields) < 3 {
			return errors.New("usage: /send <peer> <message>")
		}
		peer := fields[1]
		message := strings.TrimSpace(strings.TrimPrefix(line, "/send "+peer))
		return sendToPeer(ctx, client, state, statePath, registry, peer, message, persistMu)
	case "/quit", "/exit":
		return context.Canceled
	default:
		return fmt.Errorf("unknown command: %s", fields[0])
	}
	return nil
}

// sendToCurrent sends one line to the currently selected peer.
func sendToCurrent(ctx context.Context, client *Client, registry *ChatRegistry, text string) error {
	peer, token, ok := registry.Current()
	if !ok || peer == "" {
		return errors.New("no current peer, wait for a message or use /users then /use")
	}
	if token == "" {
		return errors.New("current peer has no context token yet")
	}
	return client.SendText(ctx, peer, text, token)
}

// sendToPeer sends one line to an explicit peer.
func sendToPeer(ctx context.Context, client *Client, state *SessionState, statePath string, registry *ChatRegistry, peer, text string, persistMu *sync.Mutex) error {
	token, ok := registry.Token(peer)
	if !ok || token == "" {
		return fmt.Errorf("peer %s has no cached context token yet", peer)
	}
	if err := registry.SetCurrent(peer); err != nil {
		return err
	}
	if err := persistState(statePath, state, registry, persistMu); err != nil {
		return err
	}
	return client.SendText(ctx, peer, text, token)
}

// extractMessageText picks the first text segment from an inbound message.
func extractMessageText(msg WeixinMessage) string {
	for _, item := range msg.ItemList {
		if item.Type == 1 && item.TextItem != nil {
			return strings.TrimSpace(item.TextItem.Text)
		}
		if item.Type == 3 && item.VoiceItem != nil {
			return strings.TrimSpace(item.VoiceItem.Text)
		}
	}
	return ""
}

// summarizeMessage provides a readable fallback for non-text messages.
func summarizeMessage(msg WeixinMessage) string {
	if len(msg.ItemList) == 0 {
		return "[empty message]"
	}

	kinds := make([]string, 0, len(msg.ItemList))
	for _, item := range msg.ItemList {
		switch item.Type {
		case 1:
			kinds = append(kinds, "text")
		case 2:
			kinds = append(kinds, "image")
		case 3:
			kinds = append(kinds, "voice")
		case 4:
			kinds = append(kinds, "file")
		case 5:
			kinds = append(kinds, "video")
		default:
			kinds = append(kinds, fmt.Sprintf("type-%d", item.Type))
		}
	}
	return "[" + strings.Join(kinds, ", ") + "]"
}

// logUsage prints the top-level CLI usage.
func logUsage() {
	logger.Info("用法")
	logger.Info("命令", "value", "go run . login [-state session.json] [-base-url https://ilinkai.weixin.qq.com] [-qr login-qr.png]")
	logger.Info("命令", "value", "go run . chat  [-state session.json]")
	logger.Info("命令", "value", "go run . [-state session.json] [-base-url ...] [-qr login-qr.png]")
}

// logChatHelp prints the interactive chat commands.
func logChatHelp() {
	logger.Info("聊天命令")
	logger.Info("命令", "value", "/help              show this help")
	logger.Info("命令", "value", "/users             list peers seen in inbound messages")
	logger.Info("命令", "value", "/who               show the current peer")
	logger.Info("命令", "value", "/use <peer>        switch the current peer")
	logger.Info("命令", "value", "/send <peer> <m>   send to a specific peer")
	logger.Info("命令", "value", "/quit              exit chat mode")
	logger.Info("提示", "value", "直接输入文本会发给当前选中的 peer")
}

// saveQRCodeImage renders the QR payload to a local PNG file.
func saveQRCodeImage(path string, content string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return "", err
	}
	if err := qrcode.WriteFile(content, qrcode.Medium, 384, absPath); err != nil {
		return "", err
	}
	return absPath, nil
}

// signalContext cancels work on Ctrl+C or SIGTERM.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	return ctx, cancel
}

// newLogger builds a text logger for terminal output.
func newLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
}

// printPromptLocked redraws the input prompt after async log output.
func printPromptLocked(registry *ChatRegistry) {
	if current, _, ok := registry.Current(); ok {
		_, _ = os.Stdout.WriteString("> [" + current + "] ")
		return
	}
	_, _ = os.Stdout.WriteString("> ")
}

// persistState saves session and cached users without letting concurrent writes race.
func persistState(statePath string, state *SessionState, registry *ChatRegistry, persistMu *sync.Mutex) error {
	persistMu.Lock()
	defer persistMu.Unlock()

	registry.ApplyToState(state)
	return SaveState(statePath, state)
}
