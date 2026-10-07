package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestReportStatusFiveInJSONAndMultipart(t *testing.T) {
	for _, multipart := range []bool{false, true} {
		name := "json"
		if multipart {
			name = "multipart"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if multipart {
					if err := r.ParseMultipartForm(maxReportPreviewSize); err != nil {
						t.Error(err)
					}
					if r.FormValue("status") != "5" || r.FormValue("code") != "ABC-123" {
						t.Error("wrong multipart status")
					}
					file, _, err := r.FormFile("previewFile")
					if err != nil {
						t.Error(err)
					} else {
						file.Close()
					}
				} else {
					var data struct {
						Code   string
						Status int
					}
					if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
						t.Error(err)
					}
					if data.Code != "ABC-123" || data.Status != 5 {
						t.Errorf("wrong JSON %+v", data)
					}
				}
				w.WriteHeader(http.StatusCreated)
			}))
			defer server.Close()
			reporter := newFilmReporter(reportOptions{URL: server.URL, Timeout: time.Second})
			status := 5
			payload := reportPayload{Code: "ABC-123", Status: &status}
			if multipart {
				preview, err := loadReportPreview(writeTestJPEG(t))
				if err != nil {
					t.Fatal(err)
				}
				payload.Preview = preview
			}
			if _, err := reporter.send(context.Background(), payload); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReportOptionalStatusUsesServerDefaultAndAcceptsOneToFive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data map[string]any
		if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
			t.Error(err)
		}
		if value, ok := data["status"]; ok && (value.(float64) < 1 || value.(float64) > 5) {
			t.Error("invalid status sent")
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	reporter := newFilmReporter(reportOptions{URL: server.URL, Timeout: time.Second})
	if _, err := reporter.send(context.Background(), reportPayload{Code: "ABC-123"}); err != nil {
		t.Fatal(err)
	}
	for status := 1; status <= 5; status++ {
		if _, err := reporter.send(context.Background(), reportPayload{Code: "ABC-123", Status: &status}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReportRejectsInvalidStatusBeforeNetworkRequest(t *testing.T) {
	reporter := newFilmReporter(reportOptions{URL: "http://unused.test", Timeout: time.Second})
	for _, status := range []int{-1, 0, 6, 99} {
		_, err := reporter.send(context.Background(), reportPayload{Code: "ABC-123", Status: &status})
		if httpErr, ok := err.(*reportHTTPError); !ok || httpErr.StatusCode != 400 {
			t.Fatalf("status %d: %v", status, err)
		}
	}
}
