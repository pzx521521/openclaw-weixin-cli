package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultBaseURL         = "https://ilinkai.weixin.qq.com"
	defaultBotType         = "3"
	defaultLongPollTimeout = 35 * time.Second
)

// Client talks to the Weixin iLink HTTP API.
type Client struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

// QRCodeResponse carries the initial QR payload used for login.
type QRCodeResponse struct {
	QRCode           string `json:"qrcode"`
	QRCodeImgContent string `json:"qrcode_img_content"`
}

// QRStatusResponse reports the current QR login state.
type QRStatusResponse struct {
	Status      string `json:"status"`
	BotToken    string `json:"bot_token"`
	ILinkBotID  string `json:"ilink_bot_id"`
	BaseURL     string `json:"baseurl"`
	ILinkUserID string `json:"ilink_user_id"`
}

// BaseInfo is attached to every JSON request.
type BaseInfo struct {
	ChannelVersion string `json:"channel_version"`
}

// GetUpdatesRequest is the long-poll request body.
type GetUpdatesRequest struct {
	GetUpdatesBuf string   `json:"get_updates_buf"`
	BaseInfo      BaseInfo `json:"base_info"`
}

// GetUpdatesResponse is the long-poll response body.
type GetUpdatesResponse struct {
	Ret                  int             `json:"ret"`
	ErrCode              int             `json:"errcode,omitempty"`
	ErrMsg               string          `json:"errmsg,omitempty"`
	Msgs                 []WeixinMessage `json:"msgs,omitempty"`
	GetUpdatesBuf        string          `json:"get_updates_buf,omitempty"`
	LongPollingTimeoutMS int             `json:"longpolling_timeout_ms,omitempty"`
}

// SendMessageRequest wraps one outbound message.
type SendMessageRequest struct {
	Msg WeixinMessage `json:"msg"`
}

// WeixinMessage is the main upstream/downstream message type.
type WeixinMessage struct {
	Seq          int           `json:"seq,omitempty"`
	MessageID    int64         `json:"message_id,omitempty"`
	FromUserID   string        `json:"from_user_id,omitempty"`
	ToUserID     string        `json:"to_user_id,omitempty"`
	ClientID     string        `json:"client_id,omitempty"`
	CreateTimeMS int64         `json:"create_time_ms,omitempty"`
	MessageType  int           `json:"message_type,omitempty"`
	MessageState int           `json:"message_state,omitempty"`
	ContextToken string        `json:"context_token,omitempty"`
	ItemList     []MessageItem `json:"item_list,omitempty"`
}

// MessageItem is one message segment such as text or image.
type MessageItem struct {
	Type      int        `json:"type,omitempty"`
	TextItem  *TextItem  `json:"text_item,omitempty"`
	VoiceItem *VoiceItem `json:"voice_item,omitempty"`
	ImageItem any        `json:"image_item,omitempty"`
	FileItem  any        `json:"file_item,omitempty"`
	VideoItem any        `json:"video_item,omitempty"`
}

// TextItem carries plain text content.
type TextItem struct {
	Text string `json:"text"`
}

// VoiceItem carries optional speech-to-text content.
type VoiceItem struct {
	Text string `json:"text,omitempty"`
}

// NewClient builds an API client with sane defaults.
func NewClient(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   strings.TrimSpace(token),
		HTTPClient: &http.Client{
			Timeout: defaultLongPollTimeout + 5*time.Second,
		},
	}
}

// FetchLoginQRCode starts a new QR login session.
func (c *Client) FetchLoginQRCode(ctx context.Context, botType string) (*QRCodeResponse, error) {
	endpoint := fmt.Sprintf("%s/ilink/bot/get_bot_qrcode?bot_type=%s", c.BaseURL, botType)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("get_bot_qrcode http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var out QRCodeResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PollLoginStatus checks whether the QR login has been scanned and confirmed.
func (c *Client) PollLoginStatus(ctx context.Context, qrcode string) (*QRStatusResponse, error) {
	endpoint := fmt.Sprintf("%s/ilink/bot/get_qrcode_status?qrcode=%s", c.BaseURL, qrcode)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("iLink-App-ClientVersion", "1")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("get_qrcode_status http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var out QRStatusResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetUpdates performs one HTTP long-poll request for inbound messages.
func (c *Client) GetUpdates(ctx context.Context, buf string, channelVersion string, timeout time.Duration) (*GetUpdatesResponse, error) {
	if timeout <= 0 {
		timeout = defaultLongPollTimeout
	}

	body := GetUpdatesRequest{
		GetUpdatesBuf: buf,
		BaseInfo: BaseInfo{
			ChannelVersion: channelVersion,
		},
	}

	var out GetUpdatesResponse
	err := c.postJSON(ctx, "/ilink/bot/getupdates", body, &out, timeout)
	if err != nil {
		if isTimeoutError(err) {
			return &GetUpdatesResponse{Ret: 0, Msgs: nil, GetUpdatesBuf: buf}, nil
		}
		return nil, err
	}
	return &out, nil
}

// SendText sends one plain text message to a Weixin user.
func (c *Client) SendText(ctx context.Context, toUserID, text, contextToken string) error {
	req := SendMessageRequest{
		Msg: WeixinMessage{
			FromUserID:   "",
			ToUserID:     toUserID,
			ClientID:     generateClientID(),
			MessageType:  2,
			MessageState: 2,
			ContextToken: contextToken,
			ItemList: []MessageItem{
				{
					Type: 1,
					TextItem: &TextItem{
						Text: text,
					},
				},
			},
		},
	}

	return c.postJSON(ctx, "/ilink/bot/sendmessage", req, nil, 15*time.Second)
}

// postJSON issues a JSON POST with the required Weixin headers.
func (c *Client) postJSON(ctx context.Context, path string, payload any, out any, timeout time.Duration) error {
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	requestCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.BaseURL+path, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}

	headers, err := c.buildHeaders(bodyBytes)
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s http %d: %s", path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// buildHeaders constructs the authentication headers expected by the API.
func (c *Client) buildHeaders(body []byte) (map[string]string, error) {
	uin, err := randomWechatUIN()
	if err != nil {
		return nil, err
	}

	headers := map[string]string{
		"Content-Type":      "application/json",
		"AuthorizationType": "ilink_bot_token",
		"Content-Length":    fmt.Sprintf("%d", len(body)),
		"X-WECHAT-UIN":      uin,
	}
	if c.Token != "" {
		headers["Authorization"] = "Bearer " + c.Token
	}
	return headers, nil
}

// randomWechatUIN matches the plugin's random uint32 then base64 logic.
func randomWechatUIN() (string, error) {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	number := binary.BigEndian.Uint32(raw[:])
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%d", number))), nil
}

// generateClientID creates a stable-enough client side message ID.
func generateClientID() string {
	return fmt.Sprintf("wechat-%d", time.Now().UnixNano())
}

// isTimeoutError treats HTTP context deadlines as normal long-poll timeouts.
func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if err == context.DeadlineExceeded {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "context deadline exceeded")
}
