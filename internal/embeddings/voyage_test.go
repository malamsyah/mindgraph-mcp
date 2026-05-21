package embeddings

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestVoyageClient_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer abc" {
			t.Errorf("auth header missing: %q", r.Header.Get("Authorization"))
		}
		var body voyageReq
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Model != "voyage-3-large" || body.InputType != "document" || body.OutputDimension != 4 {
			t.Errorf("unexpected request body: %+v", body)
		}
		resp := voyageResp{Data: []voyageRespItem{
			{Index: 0, Embedding: []float32{0.1, 0.2, 0.3, 0.4}},
			{Index: 1, Embedding: []float32{0.5, 0.6, 0.7, 0.8}},
		}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewVoyageClient("abc", "voyage-3-large", 4, nil).WithEndpoint(srv.URL)
	vecs, err := c.Embed(context.Background(), []string{"a", "b"}, InputDocument)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 2 || vecs[0][0] != 0.1 || vecs[1][3] != 0.8 {
		t.Errorf("unexpected vecs: %+v", vecs)
	}
}

func TestVoyageClient_RetriesOn5xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			http.Error(w, "transient", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(voyageResp{Data: []voyageRespItem{
			{Index: 0, Embedding: []float32{1, 2, 3, 4}},
		}})
	}))
	defer srv.Close()

	c := NewVoyageClient("k", "voyage-3-large", 4, nil).
		WithEndpoint(srv.URL).
		WithBackoff(time.Millisecond)
	vecs, err := c.Embed(context.Background(), []string{"a"}, InputQuery)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("expected 3 calls, got %d", calls.Load())
	}
	if len(vecs) != 1 || vecs[0][0] != 1 {
		t.Errorf("vecs = %+v", vecs)
	}
}

func TestVoyageClient_NonRetryable4xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	c := NewVoyageClient("k", "voyage-3-large", 4, nil).
		WithEndpoint(srv.URL).
		WithBackoff(time.Millisecond)
	if _, err := c.Embed(context.Background(), []string{"a"}, InputDocument); err == nil {
		t.Fatal("expected error on 400")
	}
	if calls.Load() != 1 {
		t.Errorf("expected 1 call (no retry on 4xx), got %d", calls.Load())
	}
}

func TestVoyageClient_MissingAPIKey(t *testing.T) {
	c := NewVoyageClient("", "voyage-3-large", 4, nil)
	if _, err := c.Embed(context.Background(), []string{"a"}, InputDocument); err == nil {
		t.Fatal("expected error when api key is empty")
	}
}

func TestVoyageClient_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	c := NewVoyageClient("k", "voyage-3-large", 4, nil).
		WithEndpoint(srv.URL).
		WithBackoff(time.Millisecond)
	if _, err := c.Embed(context.Background(), []string{"a"}, InputDocument); err == nil {
		t.Fatal("expected error on malformed JSON")
	}
}
