package cmd

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
)

type qbitTorrent struct {
	Hash    string `json:"hash"`
	Tags    string `json:"tags"`
	State   string `json:"state"`
	AddedOn int64  `json:"added_on"`
}

type qbitFile struct {
	Index    int    `json:"index"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Priority int    `json:"priority"`
}

type qbitAPIError struct {
	Endpoint string
	Status   int
}

func (e *qbitAPIError) Error() string {
	return fmt.Sprintf("qBittorrent %s 返回 HTTP %d", e.Endpoint, e.Status)
}

type qbitClient struct {
	baseURL       *url.URL
	http          *http.Client
	username      string
	password      string
	authenticated bool
	major         int
}

func newQbitClient(opts torrentFilterOptions) (*qbitClient, error) {
	base, err := url.Parse(opts.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("解析 qBittorrent 地址失败")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &qbitClient{
		baseURL: base, username: opts.Username, password: opts.Password,
		http: &http.Client{Jar: jar, Timeout: opts.RequestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

func (c *qbitClient) rawRequest(ctx context.Context, endpoint string, values url.Values, post bool) ([]byte, string, error) {
	target := *c.baseURL
	target.Path = strings.TrimRight(target.Path, "/") + "/api/v2/" + endpoint
	target.RawPath = ""
	method := http.MethodGet
	var body io.Reader
	if post {
		method = http.MethodPost
		body = strings.NewReader(values.Encode())
	} else {
		target.RawQuery = values.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, "", err
	}
	origin := c.baseURL.Scheme + "://" + c.baseURL.Host
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	if post {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("请求 qBittorrent %s 失败: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Do not log arbitrary response bodies, which may echo credentials.
		return nil, "", &qbitAPIError{Endpoint: endpoint, Status: resp.StatusCode}
	}
	const limit = 16 * 1024 * 1024
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", fmt.Errorf("读取 qBittorrent %s 响应失败: %w", endpoint, err)
	}
	if len(data) > limit {
		return nil, "", fmt.Errorf("qBittorrent %s 响应超过 16MiB", endpoint)
	}
	return data, resp.Header.Get("Content-Type"), nil
}

func (c *qbitClient) login(ctx context.Context) error {
	if c.username != "" {
		data, _, err := c.rawRequest(ctx, "auth/login", url.Values{
			"username": {c.username}, "password": {c.password},
		}, true)
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(data)) != "Ok." {
			return fmt.Errorf("qBittorrent 登录失败，请检查用户名和密码")
		}
	}
	c.authenticated = true
	return nil
}

func (c *qbitClient) request(ctx context.Context, endpoint string, values url.Values, post bool) ([]byte, string, error) {
	if !c.authenticated {
		if err := c.login(ctx); err != nil {
			return nil, "", err
		}
	}
	data, contentType, err := c.rawRequest(ctx, endpoint, values, post)
	var apiErr *qbitAPIError
	if errors.As(err, &apiErr) && (apiErr.Status == 401 || apiErr.Status == 403) && c.username != "" {
		// An authentication rejection has not performed the requested mutation.
		c.authenticated = false
		if err := c.login(ctx); err != nil {
			return nil, "", err
		}
		return c.rawRequest(ctx, endpoint, values, post)
	}
	return data, contentType, err
}

func (c *qbitClient) refreshVersion(ctx context.Context) error {
	data, _, err := c.request(ctx, "app/version", nil, false)
	if err != nil {
		return err
	}
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(string(data)), "v"), ".")
	if len(parts) < 2 {
		return fmt.Errorf("qBittorrent 返回了无效版本号")
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return fmt.Errorf("qBittorrent 返回了无效版本号")
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil || (major != 4 && major != 5) || (major == 4 && minor < 5) {
		return fmt.Errorf("torrent-filter 需要 qBittorrent 4.5+ 或 5.x")
	}
	c.major = major
	return nil
}

func (c *qbitClient) getArray(ctx context.Context, endpoint string, query url.Values, target any) error {
	data, contentType, err := c.request(ctx, endpoint, query, false)
	if err != nil {
		return err
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "application/json" || !strings.HasPrefix(strings.TrimSpace(string(data)), "[") {
		return fmt.Errorf("qBittorrent %s 未返回 JSON 数组", endpoint)
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("解析 qBittorrent %s JSON 失败: %w", endpoint, err)
	}
	return nil
}

func (c *qbitClient) torrents(ctx context.Context, query url.Values) ([]qbitTorrent, error) {
	var torrents []qbitTorrent
	if err := c.getArray(ctx, "torrents/info", query, &torrents); err != nil {
		return nil, err
	}
	return torrents, nil
}

func (c *qbitClient) torrent(ctx context.Context, hash string) (qbitTorrent, error) {
	torrents, err := c.torrents(ctx, url.Values{"hashes": {hash}})
	if err != nil {
		return qbitTorrent{}, err
	}
	if len(torrents) != 1 || torrents[0].Hash != hash {
		return qbitTorrent{}, fmt.Errorf("未找到唯一的 qBittorrent 任务 %s", hash)
	}
	return torrents[0], nil
}

func (c *qbitClient) files(ctx context.Context, hash string) ([]qbitFile, error) {
	var wire []struct {
		Index    *int   `json:"index"`
		Name     string `json:"name"`
		Size     *int64 `json:"size"`
		Priority *int   `json:"priority"`
	}
	if err := c.getArray(ctx, "torrents/files", url.Values{"hash": {hash}}, &wire); err != nil {
		return nil, err
	}
	files := make([]qbitFile, 0, len(wire))
	indexes := make(map[int]bool)
	for _, f := range wire {
		if f.Index == nil || f.Size == nil || f.Priority == nil || f.Name == "" ||
			*f.Index < 0 || *f.Size < 0 || *f.Priority < 0 || *f.Priority > 7 || indexes[*f.Index] {
			return nil, fmt.Errorf("qBittorrent 文件列表字段不完整或索引重复")
		}
		indexes[*f.Index] = true
		files = append(files, qbitFile{Index: *f.Index, Name: f.Name, Size: *f.Size, Priority: *f.Priority})
	}
	return files, nil
}

func (c *qbitClient) exclude(ctx context.Context, hash string, indexes []int) error {
	ids := make([]string, len(indexes))
	for i, index := range indexes {
		ids[i] = strconv.Itoa(index)
	}
	_, _, err := c.request(ctx, "torrents/filePrio", url.Values{
		"hash": {hash}, "id": {strings.Join(ids, "|")}, "priority": {"0"},
	}, true)
	return err
}

func (c *qbitClient) setRunning(ctx context.Context, hash string, running bool) error {
	action := "stop"
	if running {
		action = "start"
	}
	if c.major == 4 {
		action = "pause"
		if running {
			action = "resume"
		}
	}
	_, _, err := c.request(ctx, "torrents/"+action, url.Values{"hashes": {hash}}, true)
	return err
}

func validTorrentHash(hash string) bool {
	if len(hash) != 40 && len(hash) != 64 {
		return false
	}
	_, err := hex.DecodeString(hash)
	return err == nil
}

func torrentHasTag(t qbitTorrent, tag string) bool {
	for _, value := range strings.Split(t.Tags, ",") {
		if strings.TrimSpace(value) == tag {
			return true
		}
	}
	return false
}

func torrentIsStopped(t qbitTorrent) bool {
	return t.State == "pausedDL" || t.State == "pausedUP" || t.State == "stoppedDL" || t.State == "stoppedUP"
}

func torrentIsRunning(t qbitTorrent) bool {
	switch t.State {
	case "downloading", "stalledDL", "queuedDL", "forcedDL", "uploading", "stalledUP", "queuedUP", "forcedUP":
		return true
	default:
		return false
	}
}
