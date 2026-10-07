package cmd

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func nativeJSON(w http.ResponseWriter, data any) {
	json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": data})
}

func TestUGREENLoginEncryptsPasswordAndPreservesLargeAccountID(t *testing.T) {
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&private.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	public := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ugreen/v1/verify/check":
			w.Header().Set("X-Rsa-Token", public)
			nativeJSON(w, map[string]any{})
		case "/ugreen/v1/verify/login":
			var body struct{ Username, Password string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			cipher, err := base64.StdEncoding.DecodeString(body.Password)
			if err != nil {
				t.Error(err)
			}
			plain, err := rsa.DecryptPKCS1v15(rand.Reader, private, cipher)
			if err != nil || string(plain) != "fake-password" || body.Username != "fake-user" {
				t.Error("password encryption mismatch")
			}
			nativeJSON(w, map[string]any{"token": "fake-session"})
		case "/ugreen/v3/netDisk/server/conn/list":
			if r.URL.Query().Get("token") != "fake-session" || r.Header.Get("X-Ugreen-Security-Key") == "" {
				t.Error("missing session authentication")
			}
			nativeJSON(w, map[string]any{"result": []map[string]any{{"backend_type": 0, "uk": json.Number("9007199254740993"), "access_token": "fake-oauth", "device_id": "fake-device"}}})
		case "/ugreen/v3/netDisk/server/userInfo":
			if r.URL.Query().Get("uk") != "9007199254740993" {
				t.Error("account id lost precision")
			}
			nativeJSON(w, map[string]any{"connect_state": 1})
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	hash := sha256.Sum256(server.Certificate().Raw)
	client := newUGREENClient(uploadOptions{BaseURL: server.URL, CertificateSHA256: hex.EncodeToString(hash[:])})
	defer client.http.CloseIdleConnections()
	if err := client.login(context.Background(), "fake-user", "fake-password"); err != nil {
		t.Fatal(err)
	}
}

func TestUGREENRejectsChangedCertificateWithoutLeakingSession(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { nativeJSON(w, map[string]any{}) }))
	defer server.Close()
	client := newUGREENClient(uploadOptions{BaseURL: server.URL, CertificateSHA256: strings.Repeat("0", 64)})
	client.token = "must-not-appear-in-error"
	_, err := client.call(context.Background(), "v3/netDisk/server/conn/list", nil, nil, true, nil)
	if err == nil || strings.Contains(err.Error(), client.token) {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestNativeCompletionRequiresSuccessAndAllBytes(t *testing.T) {
	good := nativeUploadTask{ID: "task", Status: 5, Size: 64, Bytes: 64, Files: 1, TransferredFiles: 1}
	if err := validateNativeCompletion(&good, 64); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*nativeUploadTask){
		func(t *nativeUploadTask) { t.Status = 3 }, func(t *nativeUploadTask) { t.Size = 63 }, func(t *nativeUploadTask) { t.Bytes = 63 },
		func(t *nativeUploadTask) { t.Files = 2 }, func(t *nativeUploadTask) { t.TransferredFiles = 0 }, func(t *nativeUploadTask) { t.Errors = 1 }, func(t *nativeUploadTask) { t.ErrorCode = 1 },
	} {
		copy := good
		change(&copy)
		if err := validateNativeCompletion(&copy, 64); err == nil {
			t.Fatalf("accepted incomplete record %+v", copy)
		}
	}
}

func TestNativeQueryEscapesPathsAndReconciliationIgnoresOldTasks(t *testing.T) {
	a := &archiveJobFile{Path: filepath.Join(t.TempDir(), "movie's.7z"), PreviousTaskIDs: []string{"old"}, SubmissionIntent: time.Now()}
	job := &archiveJob{RemoteDir: "/folder's"}
	query := nativeTaskQuery(a, job, "123'456")
	if !strings.Contains(query, "movie''s.7z") || !strings.Contains(query, "123''456") || !strings.Contains(query, "/folder''s") {
		t.Fatal("unsafe SQL literals")
	}
	tasks := []nativeUploadTask{{ID: "old", Status: 5}, {ID: "new", Status: 3}}
	if task := reconcileNativeTask(a, tasks); task == nil || task.ID != "new" {
		t.Fatal("selected an old upload receipt")
	}
	a.NativeTaskID = "new"
	if task := reconcileNativeTask(a, []nativeUploadTask{{ID: "unrelated"}, {ID: "new"}}); task == nil || task.ID != "new" {
		t.Fatal("lost pinned native task")
	}
}

func nativeWaitFixture(t *testing.T, handler http.HandlerFunc) (*ugreenClient, *archiveJobFile, *archiveJob, uploadOptions) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	archive := filepath.Join(t.TempDir(), "ABC-123.tgz")
	mustWriteFile(t, archive, "test-archive")
	size, hash, err := hashRegularFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	a := &archiveJobFile{Path: archive, Size: size, SHA256: hash, Stage: "ready"}
	job := &archiveJob{RemoteDir: "/archive"}
	client := &ugreenClient{base: server.URL + "/ugreen/", http: server.Client(), clientID: "test", token: "fake-session", auth: map[string]any{"uk": "fake-account", "backend_type": 0}}
	opts := uploadOptions{PollInterval: time.Millisecond, RetryInterval: 12 * time.Hour}
	return client, a, job, opts
}

func TestNativeSubmissionIntentIsSavedBeforePOSTAndCompletionIsVerified(t *testing.T) {
	var saved, posted atomic.Bool
	client, a, job, opts := nativeWaitFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ugreen/v3/netDisk/server/file/upload":
			if !saved.Load() {
				t.Error("POST happened before intent was persisted")
			}
			posted.Store(true)
			nativeJSON(w, map[string]any{})
		case "/ugreen/v3/netDisk/server/file/list":
			var files []map[string]any
			if posted.Load() {
				files = []map[string]any{{"net_file_name": "ABC-123.tgz", "net_file_size": 12}}
			}
			nativeJSON(w, map[string]any{"net_file_list": files})
		default:
			t.Error("unexpected request")
			w.WriteHeader(404)
		}
	})
	reader := func(context.Context, uploadOptions, *archiveJobFile, *archiveJob, string) ([]nativeUploadTask, error) {
		if posted.Load() {
			return []nativeUploadTask{{ID: "new", Status: 5, Size: a.Size, Bytes: a.Size, Files: 1, TransferredFiles: 1}}, nil
		}
		return []nativeUploadTask{{ID: "old", Status: 5}}, nil
	}
	save := func() error {
		if a.SubmissionIntent.IsZero() {
			t.Error("missing submission intent")
		}
		saved.Store(true)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitUGREENUpload(ctx, opts, client, a, job, save, reader); err != nil {
		t.Fatal(err)
	}
	if a.NativeTaskID != "new" || len(a.PreviousTaskIDs) != 1 || a.PreviousTaskIDs[0] != "old" {
		t.Fatalf("receipt state: %+v", a)
	}
}

func TestInterruptedPOSTReconcilesExistingNativeTaskInsteadOfResubmitting(t *testing.T) {
	var posts atomic.Int32
	client, a, job, opts := nativeWaitFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/upload") {
			posts.Add(1)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		var files []map[string]any
		if posts.Load() > 0 {
			files = []map[string]any{{"net_file_name": "ABC-123.tgz", "net_file_size": 12}}
		}
		nativeJSON(w, map[string]any{"net_file_list": files})
	})
	reader := func(context.Context, uploadOptions, *archiveJobFile, *archiveJob, string) ([]nativeUploadTask, error) {
		if posts.Load() > 0 {
			return []nativeUploadTask{{ID: "accepted-before-interruption", Status: 5, Size: a.Size, Bytes: a.Size, Files: 1, TransferredFiles: 1}}, nil
		}
		return nil, nil
	}
	save := func() error { return nil }
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitUGREENUpload(ctx, opts, client, a, job, save, reader); err == nil {
		t.Fatal("expected ambiguous failed response")
	}
	if a.SubmissionIntent.IsZero() {
		t.Fatal("lost intent")
	}
	if err := waitUGREENUpload(ctx, opts, client, a, job, save, reader); err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 1 {
		t.Fatal("duplicate native task submitted")
	}
}

func TestCloudFileAloneDoesNotProveUploadSuccess(t *testing.T) {
	client, a, job, opts := nativeWaitFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/upload") {
			t.Error("overwrote existing cloud file")
		}
		nativeJSON(w, map[string]any{"net_file_list": []map[string]any{{"net_file_name": "ABC-123.tgz", "net_file_size": 12}}})
	})
	reader := func(context.Context, uploadOptions, *archiveJobFile, *archiveJob, string) ([]nativeUploadTask, error) {
		return nil, nil
	}
	if err := waitUGREENUpload(context.Background(), opts, client, a, job, func() error { return nil }, reader); err == nil {
		t.Fatal("accepted cloud size without native receipt")
	}
	assertExists(t, a.Path)
}
