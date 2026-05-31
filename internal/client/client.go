package client

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"time"

	"dwa/internal/pow"
)

const (
	baseURL = "https://chat.deepseek.com"
	hifURL  = "https://hif-leim.deepseek.com"
)

var commonHeaders = map[string]string{
	"Accept":                   "*/*",
	"Accept-Language":          "pl-PL,pl;q=0.9,en-US;q=0.8,en;q=0.7",
	"X-App-Version":            "2.0.0",
	"X-Client-Locale":          "pl",
	"X-Client-Platform":        "web",
	"X-Client-Timezone-Offset": "7200",
	"X-Client-Version":         "2.0.0",
}

// Chunk is one piece of streamed output from DeepSeek.
// MsgID == -1 means Text holds a content delta.
// MsgID >= 0 means this is a sentinel carrying the DeepSeek message ID.
type Chunk struct {
	Text  string
	MsgID int64
}

// DeepSeekError wraps non-2xx responses from DeepSeek.
type DeepSeekError struct{ msg string }

func (e *DeepSeekError) Error() string { return e.msg }

// Client is a thread-safe client for chat.deepseek.com.
type Client struct {
	token string
	pow   *pow.Solver
	http  *http.Client
}

func New(token string, solver *pow.Solver) *Client {
	return &Client{
		token: token,
		pow:   solver,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout: 10 * time.Second,
				}).DialContext,
				TLSHandshakeTimeout: 10 * time.Second,
			},
		},
	}
}

func (c *Client) setCommon(req *http.Request) {
	for k, v := range commonHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
}

func (c *Client) hifToken(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, hifURL+"/query", nil)
	for k, v := range commonHeaders {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var body struct {
		Data struct {
			BizData struct {
				Value string `json:"value"`
			} `json:"biz_data"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&body) //nolint:errcheck
	return body.Data.BizData.Value, nil
}

func (c *Client) powHeader(ctx context.Context, targetPath string) (string, error) {
	payload, _ := json.Marshal(map[string]string{"target_path": targetPath})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		baseURL+"/api/v0/chat/create_pow_challenge",
		strings.NewReader(string(payload)))
	c.setCommon(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var body struct {
		Data struct {
			BizData struct {
				Challenge struct {
					Challenge  string `json:"challenge"`
					Salt       string `json:"salt"`
					Difficulty int    `json:"difficulty"`
					ExpireAt   int    `json:"expire_at"`
					Algorithm  string `json:"algorithm"`
					Signature  string `json:"signature"`
				} `json:"challenge"`
			} `json:"biz_data"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&body) //nolint:errcheck
	ch := body.Data.BizData.Challenge

	answer, err := c.pow.Solve(ch.Challenge, ch.Salt, ch.Difficulty, ch.ExpireAt)
	if err != nil {
		return "", err
	}

	result := map[string]any{
		"algorithm":   ch.Algorithm,
		"challenge":   ch.Challenge,
		"salt":        ch.Salt,
		"answer":      answer,
		"signature":   ch.Signature,
		"target_path": targetPath,
	}
	encoded, _ := json.Marshal(result)
	return base64.StdEncoding.EncodeToString(encoded), nil
}

// CreateSession creates a new DeepSeek chat session and returns its ID.
func (c *Client) CreateSession(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		baseURL+"/api/v0/chat_session/create",
		strings.NewReader("{}"))
	c.setCommon(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var body struct {
		Data struct {
			BizData struct {
				ChatSession struct {
					ID string `json:"id"`
				} `json:"chat_session"`
			} `json:"biz_data"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&body) //nolint:errcheck
	return body.Data.BizData.ChatSession.ID, nil
}

// Stream yields chunks from DeepSeek via SSE.
// parentMsgID may be nil for the first message in a session.
func (c *Client) Stream(
	ctx context.Context,
	sessionID string,
	prompt string,
	parentMsgID *int64,
	thinking, search bool,
) (<-chan Chunk, <-chan error) {
	chunks := make(chan Chunk, 64)
	errc := make(chan error, 1)

	go func() {
		defer close(chunks)
		defer close(errc)

		type strRes struct {
			val string
			err error
		}
		powCh := make(chan strRes, 1)
		hifCh := make(chan strRes, 1)

		target := "/api/v0/chat/completion"

		go func() {
			v, err := c.powHeader(ctx, target)
			powCh <- strRes{v, err}
		}()
		go func() {
			v, err := c.hifToken(ctx)
			hifCh <- strRes{v, err}
		}()

		powRes := <-powCh
		hifRes := <-hifCh
		if powRes.err != nil {
			errc <- powRes.err
			return
		}
		if hifRes.err != nil {
			errc <- hifRes.err
			return
		}

		type reqPayload struct {
			ChatSessionID   string `json:"chat_session_id"`
			ParentMessageID *int64 `json:"parent_message_id"`
			ModelType       string `json:"model_type"`
			Prompt          string `json:"prompt"`
			RefFileIDs      []any  `json:"ref_file_ids"`
			ThinkingEnabled bool   `json:"thinking_enabled"`
			SearchEnabled   bool   `json:"search_enabled"`
			Preempt         bool   `json:"preempt"`
		}
		body, _ := json.Marshal(reqPayload{
			ChatSessionID:   sessionID,
			ParentMessageID: parentMsgID,
			ModelType:       "expert",
			Prompt:          prompt,
			RefFileIDs:      []any{},
			ThinkingEnabled: thinking,
			SearchEnabled:   search,
			Preempt:         false,
		})

		req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
			baseURL+target, strings.NewReader(string(body)))
		c.setCommon(req)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Ds-Pow-Response", powRes.val)
		req.Header.Set("X-Hif-Leim", hifRes.val)

		resp, err := c.http.Do(req)
		if err != nil {
			errc <- err
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			raw, _ := io.ReadAll(resp.Body)
			errc <- &DeepSeekError{fmt.Sprintf("HTTP %d: %s", resp.StatusCode, raw)}
			return
		}

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1<<20), 1<<20)
		var currentEvent string

		for scanner.Scan() {
			line := scanner.Text()

			if line == "" {
				currentEvent = ""
				continue
			}
			if strings.HasPrefix(line, "event:") {
				currentEvent = strings.TrimSpace(line[6:])
				continue
			}
			if strings.HasPrefix(line, "data:") {
				line = strings.TrimSpace(line[5:])
			}
			if line == "" || line == "[DONE]" {
				continue
			}

			var data map[string]any
			if json.Unmarshal([]byte(line), &data) != nil {
				continue
			}

			if currentEvent == "ready" {
				if mid, ok := data["response_message_id"]; ok {
					chunks <- Chunk{MsgID: toInt64(mid)}
				}
				continue
			}

			p, _ := data["p"].(string)
			o, _ := data["o"].(string)
			v := data["v"]

			switch vv := v.(type) {
			case string:
				if p == "" && o == "" && vv != "" {
					chunks <- Chunk{Text: vv, MsgID: -1}
				} else if (o == "APPEND" || o == "SET") &&
					strings.Contains(p, "content") &&
					strings.Contains(p, "fragment") &&
					vv != "" {
					chunks <- Chunk{Text: vv, MsgID: -1}
				}
			case map[string]any:
				if p != "" {
					break
				}
				respObj, ok := vv["response"].(map[string]any)
				if !ok {
					break
				}
				if mid, ok := respObj["message_id"]; ok {
					chunks <- Chunk{MsgID: toInt64(mid)}
				}
				frags, _ := respObj["fragments"].([]any)
				for _, f := range frags {
					frag, ok := f.(map[string]any)
					if !ok {
						continue
					}
					if text, ok := frag["content"].(string); ok && text != "" {
						chunks <- Chunk{Text: text, MsgID: -1}
					}
				}
			}
		}

		if err := scanner.Err(); err != nil && ctx.Err() == nil {
			errc <- err
		}
	}()

	return chunks, errc
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return -1
		}
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return -1
}
