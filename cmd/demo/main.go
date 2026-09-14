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
	"strings"
	"sync"
	"syscall"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	ilink "github.com/pzx521521/openclaw-weixin-cli/internal/ilink"
)

const (
	defaultStatePath = "session.json"
	defaultQRPath    = "login-qr.png"
)

var (
	logger      = newLogger(os.Stdout)
	errorLogger = newLogger(os.Stderr)
)

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
	baseURL := fs.String("base-url", ilink.DefaultBaseURL, "iLink API base URL")
	qrPath := fs.String("qr", defaultQRPath, "where to save the QR PNG")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if ilink.HasUsableSession(*statePath) {
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
	baseURL := fs.String("base-url", ilink.DefaultBaseURL, "iLink API base URL")
	qrPath := fs.String("qr", defaultQRPath, "where to save the QR PNG")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := signalContext()
	defer cancel()

	client := ilink.NewClient(ilink.NormalizeBaseURL(*baseURL), "")
	qrCtx, qrCancel := context.WithTimeout(ctx, 30*time.Second)
	qrResp, err := client.FetchLoginQRCode(qrCtx, ilink.DefaultBotType)
	qrCancel()
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

	state := &ilink.SessionState{
		BotToken: status.BotToken,
		BotID:    status.ILinkBotID,
		UserID:   status.ILinkUserID,
		BaseURL:  ilink.NormalizeBaseURL(status.BaseURL),
	}
	if err := ilink.SaveState(*statePath, state); err != nil {
		return err
	}

	logger.Info("登录成功", "bot_id", state.BotID, "user_id", state.UserID)
	logger.Info("session 已保存", "state", *statePath)
	logger.Info("正在进入聊天模式", "state", *statePath)
	return runChat([]string{"-state", *statePath})
}

// waitForLogin blocks until the QR login is confirmed or expires.
func waitForLogin(ctx context.Context, client *ilink.Client, qrcode string) (*ilink.QRStatusResponse, error) {
	deadline := time.Now().Add(8 * time.Minute)

	for time.Now().Before(deadline) {
		pollCtx, cancel := context.WithTimeout(ctx, ilink.DefaultLongPollTimeout)
		status, err := client.PollLoginStatus(pollCtx, qrcode)
		cancel()
		if err != nil {
			if ilink.IsTimeoutError(err) {
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
				status.BaseURL = ilink.DefaultBaseURL
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

	state, err := ilink.LoadState(*statePath)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	if state.BotToken == "" {
		return errors.New("missing bot token, run login first")
	}

	ctx, cancel := signalContext()
	defer cancel()

	client := ilink.NewClient(state.BaseURL, state.BotToken)
	registry := ilink.NewChatRegistry(state)
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
func pollLoop(ctx context.Context, client *ilink.Client, state *ilink.SessionState, statePath string, registry *ilink.ChatRegistry, ioMu *sync.Mutex, persistMu *sync.Mutex) error {
	timeout := ilink.DefaultLongPollTimeout

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		resp, err := client.GetUpdates(ctx, state.GetUpdatesBuf, ilink.ChannelVersion, timeout)
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
			if err := ilink.PersistState(statePath, state, registry, persistMu); err != nil {
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
func handleInbound(msg ilink.WeixinMessage, state *ilink.SessionState, statePath string, registry *ilink.ChatRegistry, ioMu *sync.Mutex, persistMu *sync.Mutex) error {
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
	text := ilink.ExtractMessageText(msg)
	if text == "" {
		text = ilink.SummarizeMessage(msg)
	}

	if !beforeOK || beforeToken != afterToken || beforeCurrent != afterCurrent {
		if err := ilink.PersistState(statePath, state, registry, persistMu); err != nil {
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
func inputLoop(ctx context.Context, client *ilink.Client, state *ilink.SessionState, statePath string, registry *ilink.ChatRegistry, ioMu *sync.Mutex, persistMu *sync.Mutex) error {
	lineCh := make(chan string)
	scanErrCh := make(chan error, 1)

	// Read stdin in the background so Ctrl+C can cancel the foreground loop immediately.
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			select {
			case lineCh <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			scanErrCh <- err
			return
		}
		scanErrCh <- io.EOF
	}()

	ioMu.Lock()
	printPromptLocked(registry)
	ioMu.Unlock()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-scanErrCh:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case rawLine := <-lineCh:
			line := strings.TrimSpace(rawLine)
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
	}
}

// handleCommand executes one terminal command.
func handleCommand(ctx context.Context, line string, client *ilink.Client, state *ilink.SessionState, statePath string, registry *ilink.ChatRegistry, ioMu *sync.Mutex, persistMu *sync.Mutex) error {
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
		if err := ilink.PersistState(statePath, state, registry, persistMu); err != nil {
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
func sendToCurrent(ctx context.Context, client *ilink.Client, registry *ilink.ChatRegistry, text string) error {
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
func sendToPeer(ctx context.Context, client *ilink.Client, state *ilink.SessionState, statePath string, registry *ilink.ChatRegistry, peer, text string, persistMu *sync.Mutex) error {
	token, ok := registry.Token(peer)
	if !ok || token == "" {
		return fmt.Errorf("peer %s has no cached context token yet", peer)
	}
	if err := registry.SetCurrent(peer); err != nil {
		return err
	}
	if err := ilink.PersistState(statePath, state, registry, persistMu); err != nil {
		return err
	}
	return client.SendText(ctx, peer, text, token)
}

// logUsage prints the top-level CLI usage.
func logUsage() {
	logger.Info("用法")
	logger.Info("命令", "value", "go run ./cmd/demo login [-state session.json] [-base-url https://ilinkai.weixin.qq.com] [-qr login-qr.png]")
	logger.Info("命令", "value", "go run ./cmd/demo chat  [-state session.json]")
	logger.Info("命令", "value", "go run ./cmd/demo [-state session.json] [-base-url ...] [-qr login-qr.png]")
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
func printPromptLocked(registry *ilink.ChatRegistry) {
	if current, _, ok := registry.Current(); ok {
		_, _ = os.Stdout.WriteString("> [" + current + "] ")
		return
	}
	_, _ = os.Stdout.WriteString("> ")
}
