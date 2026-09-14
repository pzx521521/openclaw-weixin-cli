package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	ilink "github.com/pzx521521/openclaw-weixin-cli/internal/ilink"
)

// msgOut is one accumulated inbound message since the last get_updates_buf.
type msgOut struct {
	From   string `json:"from"`
	Text   string `json:"text"`
	TimeMS int64  `json:"time_ms,omitempty"`
}

// result is the single JSON line printed to stdout.
type result struct {
	To    string   `json:"to,omitempty"`
	Msgs  []msgOut `json:"msgs"`
	Sent  bool     `json:"sent"`
	Error string   `json:"error,omitempty"`
}

func emit(r result) {
	if r.Msgs == nil {
		r.Msgs = []msgOut{}
	}
	raw, err := json.Marshal(r)
	if err != nil {
		fmt.Printf("{\"msgs\":[],\"sent\":false,\"error\":%q}\n", "encode result: "+err.Error())
		return
	}
	fmt.Println(string(raw))
}

func main() {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	statePath := fs.String("state", "session.json", "path to session state JSON")
	pollTimeout := fs.Duration("poll", 0, "short poll timeout for refreshing inbound messages")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}

	text := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if text == "" {
		emit(result{Sent: false, Error: "usage: send [-state session.json] [-poll 2s] <text>"})
		return
	}

	emit(run(*statePath, *pollTimeout, text))
}

func run(statePath string, pollTimeout time.Duration, text string) result {
	if pollTimeout <= 0 {
		pollTimeout = 2 * time.Second
	}

	state, err := ilink.LoadState(statePath)
	if err != nil {
		return result{Sent: false, Error: "load state: " + err.Error()}
	}
	if strings.TrimSpace(state.BotToken) == "" {
		return result{Sent: false, Error: "missing bot token, run login first"}
	}

	ctx, cancel := context.WithTimeout(context.Background(), pollTimeout+20*time.Second)
	defer cancel()

	client := ilink.NewClient(state.BaseURL, state.BotToken)
	registry := ilink.NewChatRegistry(state)
	var persistMu sync.Mutex

	resp, err := client.GetUpdates(ctx, state.GetUpdatesBuf, ilink.ChannelVersion, pollTimeout)
	if err != nil {
		return result{Sent: false, Error: "poll: " + err.Error()}
	}
	if resp.Ret != 0 || resp.ErrCode != 0 {
		return result{Sent: false, Error: fmt.Sprintf("poll: getupdates ret=%d errcode=%d errmsg=%s", resp.Ret, resp.ErrCode, resp.ErrMsg)}
	}

	msgs := make([]msgOut, 0, len(resp.Msgs))
	for _, msg := range resp.Msgs {
		from := strings.TrimSpace(msg.FromUserID)
		if from == "" {
			continue
		}
		seenAt := time.Now()
		if msg.CreateTimeMS > 0 {
			seenAt = time.UnixMilli(msg.CreateTimeMS)
		}
		registry.Upsert(from, strings.TrimSpace(msg.ContextToken), seenAt)
		itemText := ilink.ExtractMessageText(msg)
		if itemText == "" {
			itemText = ilink.SummarizeMessage(msg)
		}
		out := msgOut{From: from, Text: itemText}
		if msg.CreateTimeMS > 0 {
			out.TimeMS = msg.CreateTimeMS
		}
		msgs = append(msgs, out)
	}

	if resp.GetUpdatesBuf != "" && resp.GetUpdatesBuf != state.GetUpdatesBuf {
		state.GetUpdatesBuf = resp.GetUpdatesBuf
	}
	if err := ilink.PersistState(statePath, state, registry, &persistMu); err != nil {
		return result{Msgs: msgs, Sent: false, Error: "persist: " + err.Error()}
	}

	// 单 peer 默认：只有一个已知用户就用它，否则用当前选中的 peer。
	to := ""
	if peers := registry.List(); len(peers) == 1 {
		to = peers[0]
	} else if cur, _, ok := registry.Current(); ok {
		to = cur
	}
	if to == "" {
		return result{Msgs: msgs, Sent: false, Error: "no peer yet, wait for an inbound message"}
	}

	token, ok := registry.Token(to)
	if !ok || strings.TrimSpace(token) == "" {
		return result{Msgs: msgs, To: to, Sent: false, Error: "peer has no context token yet"}
	}

	if err := client.SendText(ctx, to, text, token); err != nil {
		return result{Msgs: msgs, To: to, Sent: false, Error: "send: " + err.Error()}
	}
	return result{Msgs: msgs, To: to, Sent: true}
}
