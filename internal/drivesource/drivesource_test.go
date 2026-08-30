package drivesource

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

type mockFile struct {
	ID           string
	Name         string
	ModifiedTime string
}

type recorder struct {
	listQuery      url.Values
	downloadCalled bool
	downloadedID   string
}

func newServer(t *testing.T, files []mockFile, data map[string][]byte) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("/files", func(w http.ResponseWriter, r *http.Request) {
		rec.listQuery = r.URL.Query()
		list := make([]map[string]string, 0, len(files))
		for _, f := range files {
			list = append(list, map[string]string{
				"id":           f.ID,
				"name":         f.Name,
				"modifiedTime": f.ModifiedTime,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"files": list})
	})
	mux.HandleFunc("/files/", func(w http.ResponseWriter, r *http.Request) {
		rec.downloadCalled = true
		id := strings.TrimPrefix(r.URL.Path, "/files/")
		rec.downloadedID = id
		d, ok := data[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(d)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, rec
}

func newErrorServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/files", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newTestSource(t *testing.T, srv *httptest.Server, folderID string) *Source {
	t.Helper()
	svc, err := drive.NewService(context.Background(),
		option.WithEndpoint(srv.URL),
		option.WithoutAuthentication(),
		option.WithHTTPClient(srv.Client()),
	)
	if err != nil {
		t.Fatalf("drive.NewService: %v", err)
	}
	return newWithService(svc, folderID)
}

func TestFetchLatest_EmptyList(t *testing.T) {
	srv, rec := newServer(t, nil, nil)
	s := newTestSource(t, srv, "folder1")

	got, err := s.FetchLatest(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
	if rec.downloadCalled {
		t.Errorf("download should not be called")
	}
}

func TestFetchLatest_NoZipFiles(t *testing.T) {
	srv, rec := newServer(t, []mockFile{
		{ID: "1", Name: "note.txt", ModifiedTime: "2024-01-01T00:00:00Z"},
	}, nil)
	s := newTestSource(t, srv, "folder1")

	got, err := s.FetchLatest(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
	if rec.downloadCalled {
		t.Errorf("download should not be called")
	}
}

func TestFetchLatest_SkipsNonZipFirst(t *testing.T) {
	srv, rec := newServer(t, []mockFile{
		{ID: "1", Name: "readme.txt", ModifiedTime: "2024-01-02T00:00:00Z"},
		{ID: "2", Name: "export.zip", ModifiedTime: "2024-01-01T00:00:00Z"},
	}, map[string][]byte{"2": []byte("zip-bytes")})
	s := newTestSource(t, srv, "folder1")

	got, err := s.FetchLatest(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatalf("expected a result")
	}
	if got.FileID != "2" || got.Name != "export.zip" {
		t.Errorf("unexpected file selected: %+v", got)
	}
	if string(got.Data) != "zip-bytes" {
		t.Errorf("unexpected data: %q", got.Data)
	}
	if rec.downloadedID != "2" {
		t.Errorf("expected download of id 2, got %q", rec.downloadedID)
	}
}

func TestFetchLatest_UppercaseExtension(t *testing.T) {
	srv, _ := newServer(t, []mockFile{
		{ID: "1", Name: "EXPORT.ZIP", ModifiedTime: "2024-01-01T00:00:00Z"},
	}, map[string][]byte{"1": []byte("data")})
	s := newTestSource(t, srv, "folder1")

	got, err := s.FetchLatest(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.FileID != "1" {
		t.Fatalf("expected file 1 selected, got %+v", got)
	}
}

func TestFetchLatest_SameModifiedTime_NotNew(t *testing.T) {
	modified := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	srv, rec := newServer(t, []mockFile{
		{ID: "1", Name: "export.zip", ModifiedTime: modified.Format(time.RFC3339)},
	}, map[string][]byte{"1": []byte("data")})
	s := newTestSource(t, srv, "folder1")

	got, err := s.FetchLatest(context.Background(), modified)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
	if rec.downloadCalled {
		t.Errorf("download should not be called when not newer")
	}
}

func TestFetchLatest_NewerThanAfter(t *testing.T) {
	after := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	modified := after.Add(time.Hour)
	srv, rec := newServer(t, []mockFile{
		{ID: "1", Name: "export.zip", ModifiedTime: modified.Format(time.RFC3339)},
	}, map[string][]byte{"1": []byte("payload")})
	s := newTestSource(t, srv, "folder1")

	got, err := s.FetchLatest(context.Background(), after)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatalf("expected result")
	}
	if !got.ModifiedTime.Equal(modified) {
		t.Errorf("unexpected modifiedTime: got %v, want %v", got.ModifiedTime, modified)
	}
	if string(got.Data) != "payload" {
		t.Errorf("unexpected data: %q", got.Data)
	}
	if !rec.downloadCalled {
		t.Errorf("expected download to be called")
	}
}

func TestFetchLatest_ZeroAfterAlwaysDownloads(t *testing.T) {
	srv, rec := newServer(t, []mockFile{
		{ID: "1", Name: "export.zip", ModifiedTime: "2000-01-01T00:00:00Z"},
	}, map[string][]byte{"1": []byte("payload")})
	s := newTestSource(t, srv, "folder1")

	got, err := s.FetchLatest(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatalf("expected result")
	}
	if !rec.downloadCalled {
		t.Errorf("expected download to be called")
	}
}

func TestFetchLatest_ListRequestParams(t *testing.T) {
	srv, rec := newServer(t, nil, nil)
	s := newTestSource(t, srv, "myFolderID")

	if _, err := s.FetchLatest(context.Background(), time.Time{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantQ := "'myFolderID' in parents and trashed = false"
	if got := rec.listQuery.Get("q"); got != wantQ {
		t.Errorf("q = %q, want %q", got, wantQ)
	}
	if got := rec.listQuery.Get("orderBy"); got != "modifiedTime desc" {
		t.Errorf("orderBy = %q, want %q", got, "modifiedTime desc")
	}
}

func TestFetchLatest_ListError(t *testing.T) {
	srv := newErrorServer(t)
	s := newTestSource(t, srv, "folder1")

	_, err := s.FetchLatest(context.Background(), time.Time{})
	if err == nil {
		t.Fatalf("expected error")
	}
}
