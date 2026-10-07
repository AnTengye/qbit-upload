package cmd

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type ugreenClient struct {
	base, clientID, token string
	http                  *http.Client
	auth                  map[string]any
}

type ugreenEnvelope struct {
	Code int             `json:"code"`
	Data json.RawMessage `json:"data"`
}

func newUGREENClient(opts uploadOptions) *ugreenClient {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if opts.CertificateSHA256 != "" {
		transport.TLSClientConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			// A configured certificate pin replaces CA verification; it does
			// not permit an arbitrary self-signed certificate.
			InsecureSkipVerify: true,
			VerifyConnection: func(state tls.ConnectionState) error {
				if len(state.PeerCertificates) == 0 {
					return fmt.Errorf("NAS 未提供 TLS 证书")
				}
				hash := sha256.Sum256(state.PeerCertificates[0].Raw)
				if hex.EncodeToString(hash[:]) != opts.CertificateSHA256 {
					return fmt.Errorf("NAS TLS 证书指纹不匹配")
				}
				return nil
			},
		}
	}
	var id [16]byte
	rand.Read(id[:])
	return &ugreenClient{base: strings.TrimRight(opts.BaseURL, "/") + "/ugreen/",
		clientID: fmt.Sprintf("%x-%x-%x-%x-WEB", id[:4], id[4:6], id[6:8], id[8:10]),
		http: &http.Client{Transport: transport, Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (c *ugreenClient) call(ctx context.Context, endpoint string, payload any, params url.Values, authenticated bool, out any) (http.Header, error) {
	if params == nil {
		params = make(url.Values)
	}
	if authenticated {
		params.Set("token", c.token)
	}
	requestURL := c.base + endpoint
	if len(params) != 0 {
		requestURL += "?" + params.Encode()
	}
	var data []byte
	var err error
	if payload != nil {
		data, err = json.Marshal(payload)
		if err != nil {
			return nil, err
		}
	}
	method := http.MethodGet
	if payload != nil {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, requestURL, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("创建 NAS 请求失败: %s", endpoint)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("UG-Client-Id", c.clientID)
	if authenticated {
		security := md5.Sum([]byte(c.token))
		req.Header.Set("X-Ugreen-Security-Key", hex.EncodeToString(security[:]))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// net/url errors contain the token-bearing URL. Never log them.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("NAS %s: %w", endpoint, ctx.Err())
		}
		return nil, fmt.Errorf("NAS %s 请求失败，请检查连接和 TLS 证书指纹", endpoint)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("NAS %s: HTTP %d", endpoint, resp.StatusCode)
	}
	var envelope ugreenEnvelope
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8*1024*1024)).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("NAS %s 响应无法解析", endpoint)
	}
	if envelope.Code != 0 && envelope.Code != 200 {
		return nil, fmt.Errorf("NAS %s: API code %d", endpoint, envelope.Code)
	}
	if out != nil && len(envelope.Data) != 0 {
		decoder := json.NewDecoder(bytes.NewReader(envelope.Data))
		decoder.UseNumber()
		if err := decoder.Decode(out); err != nil {
			return nil, fmt.Errorf("NAS %s 数据格式变化: %w", endpoint, err)
		}
	}
	return resp.Header, nil
}

func (c *ugreenClient) login(ctx context.Context, username, password string) error {
	if username == "" || password == "" {
		return fmt.Errorf("请设置 upload.username 和 QBIT_UPLOAD_NAS_PASSWORD 环境变量")
	}
	headers, err := c.call(ctx, "v1/verify/check", map[string]any{"username": username}, nil, false, nil)
	if err != nil {
		return err
	}
	keyData, err := base64.StdEncoding.DecodeString(headers.Get("X-Rsa-Token"))
	if err != nil || len(keyData) == 0 {
		return fmt.Errorf("NAS 未返回有效 RSA 公钥")
	}
	if !bytes.Contains(keyData, []byte("-----BEGIN")) {
		keyData = []byte("-----BEGIN PUBLIC KEY-----\n" + string(keyData) + "\n-----END PUBLIC KEY-----\n")
	}
	block, _ := pem.Decode(keyData)
	if block == nil {
		return fmt.Errorf("NAS RSA 公钥格式错误")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return fmt.Errorf("NAS RSA 公钥解析失败")
	}
	key, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return fmt.Errorf("NAS 公钥不是 RSA")
	}
	encrypted, err := rsa.EncryptPKCS1v15(rand.Reader, key, []byte(password))
	if err != nil {
		return fmt.Errorf("NAS 密码加密失败")
	}
	var login struct {
		Token    string `json:"token"`
		APIToken string `json:"api_token"`
	}
	_, err = c.call(ctx, "v1/verify/login", map[string]any{
		"username": username, "password": base64.StdEncoding.EncodeToString(encrypted),
		"keepalive": false, "otp": true, "is_simple": false,
	}, nil, false, &login)
	if err != nil {
		return err
	}
	c.token = login.Token
	if c.token == "" {
		c.token = login.APIToken
	}
	if c.token == "" {
		return fmt.Errorf("NAS 登录未返回会话，可能需要额外验证")
	}
	var connections struct {
		Result []map[string]any `json:"result"`
	}
	if _, err := c.call(ctx, "v3/netDisk/server/conn/list", nil, nil, true, &connections); err != nil {
		return err
	}
	var candidates []map[string]any
	for _, row := range connections.Result {
		if fmt.Sprint(row["backend_type"]) == "0" {
			candidates = append(candidates, row)
		}
	}
	if len(candidates) != 1 {
		return fmt.Errorf("需要当前 NAS 用户恰好绑定一个百度网盘账号，当前为 %d", len(candidates))
	}
	c.auth = make(map[string]any)
	for _, key := range []string{"uk", "backend_type", "access_token", "device_id"} {
		if value, ok := candidates[0][key]; ok {
			c.auth[key] = value
		}
	}
	var info struct {
		ConnectState int `json:"connect_state"`
	}
	if _, err := c.call(ctx, "v3/netDisk/server/userInfo", nil, c.authParams(), true, &info); err != nil {
		return err
	}
	if info.ConnectState != 1 {
		return fmt.Errorf("百度网盘连接不可用，请在绿联网盘工具中重新连接")
	}
	return nil
}

func (c *ugreenClient) authParams() url.Values {
	params := make(url.Values)
	for key, value := range c.auth {
		params.Set(key, fmt.Sprint(value))
	}
	return params
}

func (c *ugreenClient) authBody() map[string]any {
	body := make(map[string]any)
	for key, value := range c.auth {
		body[key] = value
	}
	return body
}

type cloudFile struct {
	Name string `json:"net_file_name"`
	Path string `json:"net_file_path"`
	Size int64  `json:"net_file_size"`
}

func (c *ugreenClient) findCloudFile(ctx context.Context, dir, name string) (*cloudFile, error) {
	next := ""
	for page := 1; page <= 10000; page++ {
		body := c.authBody()
		body["op_type"], body["net_file_path"], body["page"], body["size"] = 2, dir, page, 1000
		body["order"], body["desc"], body["next_page"] = "name", 0, next
		var result struct {
			Files []cloudFile `json:"net_file_list"`
			Next  string      `json:"next_page"`
		}
		if _, err := c.call(ctx, "v3/netDisk/server/file/list", body, nil, true, &result); err != nil {
			return nil, err
		}
		for _, file := range result.Files {
			if file.Name == name {
				return &file, nil
			}
		}
		if len(result.Files) < 1000 {
			return nil, nil
		}
		if result.Next != "" && result.Next == next {
			return nil, fmt.Errorf("网盘分页游标重复")
		}
		next = result.Next
	}
	return nil, fmt.Errorf("网盘目录分页超出上限")
}

type nativeUploadTask struct {
	ID               string `json:"task_id"`
	Status           int    `json:"status"`
	Size             int64  `json:"total_size"`
	Bytes            int64  `json:"transferred_bytes"`
	Files            int64  `json:"total_files"`
	TransferredFiles int64  `json:"transferred_files"`
	Errors           int    `json:"errors"`
	ErrorCode        int    `json:"err_code"`
}

func nativeTaskQuery(a *archiveJobFile, job *archiveJob, uk string) string {
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	return "SELECT task_id,status,total_size,transferred_bytes,total_files,transferred_files,errors,err_code FROM upload_task WHERE backend_type=0 AND is_dir=0 AND uk=" + quote(uk) +
		" AND file_name=" + quote(filepath.Base(a.Path)) + " AND local_path IN (" + quote(a.Path) + "," + quote(filepath.Dir(a.Path)) + ") AND remote_path IN (" + quote(job.RemoteDir) + "," + quote(path.Join(job.RemoteDir, filepath.Base(a.Path))) + ") ORDER BY id DESC;"
}

func readNativeUploadTasks(ctx context.Context, opts uploadOptions, a *archiveJobFile, job *archiveJob, uk string) ([]nativeUploadTask, error) {
	cmd := exec.CommandContext(ctx, opts.SQLite, "-readonly", "-json", "-cmd", ".timeout 5000", opts.TaskDB, nativeTaskQuery(a, job, uk))
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("读取绿联上传记录失败（只读），请检查 sqlite3 和任务数据库权限")
	}
	if len(bytes.TrimSpace(output)) == 0 {
		return nil, nil
	}
	var tasks []nativeUploadTask
	if err := json.Unmarshal(output, &tasks); err != nil {
		return nil, fmt.Errorf("绿联上传记录格式变化: %w", err)
	}
	return tasks, nil
}

func reconcileNativeTask(a *archiveJobFile, tasks []nativeUploadTask) *nativeUploadTask {
	for _, task := range tasks {
		if a.NativeTaskID != "" {
			if task.ID == a.NativeTaskID {
				return &task
			}
			continue
		}
		previous := false
		for _, id := range a.PreviousTaskIDs {
			if task.ID == id {
				previous = true
				break
			}
		}
		if !previous && !a.SubmissionIntent.IsZero() {
			return &task
		}
	}
	return nil
}

func validateNativeCompletion(task *nativeUploadTask, size int64) error {
	if task.Status != 5 || task.Size != size || task.Bytes != size || task.Files != 1 || task.TransferredFiles != 1 || task.Errors != 0 || task.ErrorCode != 0 {
		return fmt.Errorf("原生上传记录未确认完整成功: task=%s status=%d", task.ID, task.Status)
	}
	return nil
}

func uploadViaUGREEN(ctx context.Context, opts uploadOptions, a *archiveJobFile, job *archiveJob, save func() error) error {
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	client := newUGREENClient(opts)
	defer client.http.CloseIdleConnections()
	if err := client.login(ctx, opts.Username, os.Getenv("QBIT_UPLOAD_NAS_PASSWORD")); err != nil {
		return err
	}
	return waitUGREENUpload(ctx, opts, client, a, job, save, readNativeUploadTasks)
}

type nativeTaskReader func(context.Context, uploadOptions, *archiveJobFile, *archiveJob, string) ([]nativeUploadTask, error)

func waitUGREENUpload(ctx context.Context, opts uploadOptions, client *ugreenClient, a *archiveJobFile, job *archiveJob, save func() error, readTasks nativeTaskReader) error {
	uk := fmt.Sprint(client.auth["uk"])
	maySubmit := a.SubmissionIntent.IsZero() || time.Since(a.SubmissionIntent) >= opts.RetryInterval
	for {
		tasks, err := readTasks(ctx, opts, a, job, uk)
		if err != nil {
			return err
		}
		task := reconcileNativeTask(a, tasks)
		if task != nil {
			if a.NativeTaskID != task.ID {
				a.NativeTaskID = task.ID
				if err := save(); err != nil {
					return err
				}
			}
			switch task.Status {
			case 5:
				if err := validateNativeCompletion(task, a.Size); err != nil {
					return err
				}
				cloud, err := client.findCloudFile(ctx, job.RemoteDir, filepath.Base(a.Path))
				if err != nil {
					return err
				}
				if cloud == nil || cloud.Size != a.Size {
					return fmt.Errorf("原生任务完成，但网盘文件不存在或大小不符")
				}
				return verifyArchive(a)
			case 6, 7, 8:
				// A definitive failure permits a fresh submission on the NEXT
				// retry, not repeated submissions within this attempt.
				a.PreviousTaskIDs = append(a.PreviousTaskIDs, task.ID)
				a.NativeTaskID = ""
				a.SubmissionIntent = time.Time{}
				if err := save(); err != nil {
					return err
				}
				return fmt.Errorf("绿联上传任务失败或已删除: task=%s status=%d", task.ID, task.Status)
			case 4:
				return fmt.Errorf("绿联上传任务已暂停，请在网盘工具中恢复: %s", task.ID)
			}
		} else {
			if a.NativeTaskID != "" {
				return fmt.Errorf("已提交的绿联任务记录丢失，保留压缩包等待核查: %s", a.NativeTaskID)
			}
			if !maySubmit && time.Since(a.SubmissionIntent) >= time.Minute {
				return fmt.Errorf("上传提交结果不明确；已保留记录，下次重试先核对原生任务")
			}
			if maySubmit {
				cloud, err := client.findCloudFile(ctx, job.RemoteDir, filepath.Base(a.Path))
				if err != nil {
					return err
				}
				if cloud != nil {
					return fmt.Errorf("网盘已有同名文件，但缺少本次上传成功记录；保留压缩包，避免覆盖")
				}
				for _, old := range tasks {
					a.PreviousTaskIDs = append(a.PreviousTaskIDs, old.ID)
				}
				a.SubmissionIntent = time.Now().UTC()
				if err := save(); err != nil {
					return err
				}
				body := client.authBody()
				body["target_id"], body["target_path"], body["op_type"], body["storage_class"] = "/", job.RemoteDir, 1, ""
				body["source_path_list"] = []map[string]any{{"path": a.Path, "file_size": a.Size, "isDir": false}}
				if _, err := client.call(ctx, "v3/netDisk/server/file/upload", body, nil, true, nil); err != nil {
					return err
				}
				maySubmit = false
				stepLog("绿联上传已提交，等待完成确认: %s（%s 字节）", filepath.Base(a.Path), strconv.FormatInt(a.Size, 10))
			}
		}
		timer := time.NewTimer(opts.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("等待绿联上传完成: %w", ctx.Err())
		case <-timer.C:
		}
	}
}
