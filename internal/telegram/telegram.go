// Package telegram 把图片存到 Telegram 频道（Bot API），当作免费的图床。
//
// 上传用 sendDocument（保持原图，不会被重新压缩）；读取时用 getFile 换出下载地址，
// 由程序自己下载再转给浏览器，浏览器看不到机器人的 token。
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Client 是 Bot API 客户端。
type Client struct {
	token  string
	chatID string
	base   string
	http   *http.Client

	mu    sync.Mutex
	paths map[string]cachedPath // file_id → 下载路径（Telegram 保证至少 1 小时有效）
}

type cachedPath struct {
	path    string
	expires time.Time
}

// New 创建客户端。base 为空时用 https://api.telegram.org。
func New(token, chatID, base string, client *http.Client) *Client {
	if base == "" {
		base = "https://api.telegram.org"
	}
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{token: token, chatID: chatID, base: strings.TrimRight(base, "/"), http: client, paths: map[string]cachedPath{}}
}

// RetryAfter 表示被 Telegram 限流，要等一会儿再试。
type RetryAfter struct{ Wait time.Duration }

func (e *RetryAfter) Error() string { return fmt.Sprintf("Telegram 限流，%s 后再试", e.Wait) }

type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

func (c *Client) call(ctx context.Context, method string, body io.Reader, contentType string, out any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/bot"+c.token+"/"+method, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return redact(err, c.token)
	}
	defer response.Body.Close()
	var result apiResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return fmt.Errorf("%s: 响应不是 JSON（HTTP %d）", method, response.StatusCode)
	}
	if response.StatusCode == http.StatusTooManyRequests || result.Parameters.RetryAfter > 0 {
		return &RetryAfter{Wait: time.Duration(max(result.Parameters.RetryAfter, 1)) * time.Second}
	}
	if !result.OK {
		return fmt.Errorf("%s: %s", method, result.Description)
	}
	if out != nil {
		return json.Unmarshal(result.Result, out)
	}
	return nil
}

// redact 把错误信息里的 token 去掉（网络错误会带上请求地址）。
func redact(err error, token string) error {
	if token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "***"))
}

// Uploaded 是上传结果。取回文件只需要 FileID。
type Uploaded struct {
	FileID string
	Size   int
}

// Upload 把一个文件发到存图的会话（频道、群或和机器人的私聊）。
func (c *Client) Upload(ctx context.Context, name, contentType string, data []byte) (Uploaded, error) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	_ = form.WriteField("chat_id", c.chatID)
	_ = form.WriteField("disable_notification", "true")
	_ = form.WriteField("disable_content_type_detection", "true")
	header := make(map[string][]string)
	header["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name="document"; filename=%q`, name)}
	header["Content-Type"] = []string{contentType}
	part, err := form.CreatePart(header)
	if err != nil {
		return Uploaded{}, err
	}
	if _, err := part.Write(data); err != nil {
		return Uploaded{}, err
	}
	if err := form.Close(); err != nil {
		return Uploaded{}, err
	}
	var message struct {
		Document struct {
			FileID   string `json:"file_id"`
			FileSize int    `json:"file_size"`
		} `json:"document"`
	}
	if err := c.call(ctx, "sendDocument", &body, form.FormDataContentType(), &message); err != nil {
		return Uploaded{}, err
	}
	if message.Document.FileID == "" {
		return Uploaded{}, errors.New("sendDocument: 没有返回文件")
	}
	return Uploaded{FileID: message.Document.FileID, Size: message.Document.FileSize}, nil
}

// Open 下载一个文件。调用方负责关闭返回的 Body。
func (c *Client) Open(ctx context.Context, fileID string) (io.ReadCloser, error) {
	path, err := c.filePath(ctx, fileID)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/file/bot"+c.token+"/"+path, nil)
	if err != nil {
		return nil, err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, redact(err, c.token)
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		c.forget(fileID) // 下载地址过期了：下次重新取
		return nil, fmt.Errorf("下载文件失败（HTTP %d）", response.StatusCode)
	}
	return response.Body, nil
}

func (c *Client) filePath(ctx context.Context, fileID string) (string, error) {
	c.mu.Lock()
	cached, ok := c.paths[fileID]
	c.mu.Unlock()
	if ok && time.Now().Before(cached.expires) {
		return cached.path, nil
	}
	var file struct {
		FilePath string `json:"file_path"`
	}
	if err := c.call(ctx, "getFile", strings.NewReader(url.Values{"file_id": {fileID}}.Encode()), "application/x-www-form-urlencoded", &file); err != nil {
		return "", err
	}
	if file.FilePath == "" {
		return "", errors.New("getFile: 没有下载路径")
	}
	c.mu.Lock()
	if len(c.paths) > 20000 {
		c.paths = map[string]cachedPath{}
	}
	c.paths[fileID] = cachedPath{path: file.FilePath, expires: time.Now().Add(50 * time.Minute)}
	c.mu.Unlock()
	return file.FilePath, nil
}

func (c *Client) forget(fileID string) {
	c.mu.Lock()
	delete(c.paths, fileID)
	c.mu.Unlock()
}

// Private 表示存图的地方是和某个人的私聊（chat_id 是正数），不是频道或群。
// 私聊每秒可以发一条；频道和群每分钟大约 20 条。
func (c *Client) Private() bool { return c.chatID != "" && !strings.HasPrefix(c.chatID, "-") }

// Chat 是机器人最近收到消息的会话，用来找 chat_id。
type Chat struct {
	ID    int64
	Type  string
	Title string
}

// RecentChats 返回机器人最近收到消息的会话（getUpdates）。只读。
func (c *Client) RecentChats(ctx context.Context) ([]Chat, error) {
	var updates []struct {
		Message *struct {
			Chat struct {
				ID        int64  `json:"id"`
				Type      string `json:"type"`
				Title     string `json:"title"`
				Username  string `json:"username"`
				FirstName string `json:"first_name"`
			} `json:"chat"`
		} `json:"message"`
		ChannelPost *struct {
			Chat struct {
				ID    int64  `json:"id"`
				Type  string `json:"type"`
				Title string `json:"title"`
			} `json:"chat"`
		} `json:"channel_post"`
	}
	if err := c.call(ctx, "getUpdates", nil, "", &updates); err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	var chats []Chat
	for _, update := range updates {
		var chat Chat
		switch {
		case update.Message != nil:
			m := update.Message.Chat
			chat = Chat{ID: m.ID, Type: m.Type, Title: m.Title}
			if chat.Title == "" {
				chat.Title = strings.TrimSpace(m.FirstName + " @" + m.Username)
			}
		case update.ChannelPost != nil:
			p := update.ChannelPost.Chat
			chat = Chat{ID: p.ID, Type: p.Type, Title: p.Title}
		default:
			continue
		}
		if !seen[chat.ID] {
			seen[chat.ID] = true
			chats = append(chats, chat)
		}
	}
	return chats, nil
}

// Check 检查机器人和频道：返回机器人的用户名和频道名称。只读，不发消息。
func (c *Client) Check(ctx context.Context) (bot, chat string, err error) {
	var me struct {
		Username string `json:"username"`
	}
	if err := c.call(ctx, "getMe", nil, "", &me); err != nil {
		return "", "", err
	}
	var info struct {
		Title    string `json:"title"`
		Username string `json:"username"`
		Type     string `json:"type"`
	}
	if err := c.call(ctx, "getChat", strings.NewReader(url.Values{"chat_id": {c.chatID}}.Encode()), "application/x-www-form-urlencoded", &info); err != nil {
		return me.Username, "", err
	}
	return me.Username, info.Title + "（" + info.Type + "）", nil
}
