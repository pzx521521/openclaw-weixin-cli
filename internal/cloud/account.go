package cloud

import (
	"context"
	"fmt"
	"strings"
	"time"

	ilink "github.com/pzx521521/openclaw-weixin-cli/internal/ilink"
)

// MsgItem is one accumulated inbound message since the last get_updates_buf.
type MsgItem struct {
	From   string `json:"from"`
	Text   string `json:"text"`
	TimeMS int64  `json:"time_ms,omitempty"`
}

// shortPoll performs one bounded getupdates, persists buf/tokens, and
// returns messages accumulated since the previous buf.
func (s *Server) shortPoll(ctx context.Context, botID string, timeout time.Duration) ([]MsgItem, error) {
	msgs := make([]MsgItem, 0)
	err := s.store.WithLockedState(ctx, botID, func(state *ilink.SessionState, reg *ilink.ChatRegistry) error {
		client := ilink.NewClient(state.BaseURL, state.BotToken)
		resp, err := client.GetUpdates(ctx, state.GetUpdatesBuf, ilink.ChannelVersion, timeout)
		if err != nil {
			return fmt.Errorf("poll: %w", err)
		}
		if resp.Ret != 0 || resp.ErrCode != 0 {
			return fmt.Errorf("poll: getupdates ret=%d errcode=%d errmsg=%s", resp.Ret, resp.ErrCode, resp.ErrMsg)
		}

		if resp.GetUpdatesBuf != "" {
			state.GetUpdatesBuf = resp.GetUpdatesBuf
		}
		for _, msg := range resp.Msgs {
			from := strings.TrimSpace(msg.FromUserID)
			if from == "" {
				continue
			}
			seenAt := time.Now()
			if msg.CreateTimeMS > 0 {
				seenAt = time.UnixMilli(msg.CreateTimeMS)
			}
			reg.Upsert(from, strings.TrimSpace(msg.ContextToken), seenAt)
			text := ilink.ExtractMessageText(msg)
			if text == "" {
				text = ilink.SummarizeMessage(msg)
			}
			out := MsgItem{From: from, Text: text}
			if msg.CreateTimeMS > 0 {
				out.TimeMS = msg.CreateTimeMS
			}
			msgs = append(msgs, out)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return msgs, nil
}

// resolveTarget picks the send target: single peer by default, else current.
func (s *Server) resolveTarget(ctx context.Context, botID string) (to, token string, err error) {
	u, err := s.store.GetUser(ctx, botID)
	if err != nil {
		return "", "", err
	}
	return s.resolveTargetFromUser(u)
}

// sendText sends one text message.
// pollTimeout > 0: short-poll first to refresh buf/tokens and return backlog.
// pollTimeout == 0: pure cache send, no GetUpdates call, msgs always empty.
func (s *Server) sendText(ctx context.Context, botID, text string, pollTimeout time.Duration) (to string, msgs []MsgItem, sent bool, sendErr string) {
	if pollTimeout > 0 {
		var err error
		msgs, err = s.shortPoll(ctx, botID, pollTimeout)
		if err != nil {
			return "", msgsOrEmpty(msgs), false, err.Error()
		}
	}

	u, err := s.store.GetUser(ctx, botID)
	if err != nil {
		return "", msgsOrEmpty(msgs), false, err.Error()
	}
	to, token, err := s.resolveTargetFromUser(u)
	if err != nil {
		return "", msgsOrEmpty(msgs), false, err.Error()
	}

	client := ilink.NewClient(u.BaseURL, u.BotToken)
	if err := client.SendText(ctx, to, text, token); err != nil {
		return to, msgsOrEmpty(msgs), false, "send: " + err.Error()
	}
	return to, msgsOrEmpty(msgs), true, ""
}

func msgsOrEmpty(msgs []MsgItem) []MsgItem {
	if msgs == nil {
		return []MsgItem{}
	}
	return msgs
}

// resolveTargetFromUser resolves the target from an already loaded row.
func (s *Server) resolveTargetFromUser(u *UserRow) (to, token string, err error) {
	reg := ilink.NewChatRegistry(u.ToState())
	if peers := reg.List(); len(peers) == 1 {
		to = peers[0]
	} else if cur, _, ok := reg.Current(); ok {
		to = cur
	}
	if to == "" {
		return "", "", fmt.Errorf("no peer yet, send a WeChat message first")
	}
	token, ok := reg.Token(to)
	if !ok || strings.TrimSpace(token) == "" {
		return "", "", fmt.Errorf("peer has no context token yet")
	}
	return to, token, nil
}
