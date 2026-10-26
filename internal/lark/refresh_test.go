package lark

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type refreshTransport func(*http.Request) (*http.Response, error)

func (f refreshTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(body string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestTaskRefreshCancellationAndAPIErrors(t *testing.T) {
	client := New(Config{AppID: "id", AppSecret: "secret"})
	client.token = "token"
	client.tokenExp = time.Now().Add(time.Hour)
	client.httpClient.Transport = refreshTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			return response(`{"code":0,"data":{"task":{"guid":"task","completed_at":"1234"}}}`), nil
		}
		return response(`{"code":999,"msg":"write denied"}`), nil
	})
	task, err := client.GetTaskContext(context.Background(), "task")
	if err != nil || !task.Completed {
		t.Fatalf("completion not decoded: %+v %v", task, err)
	}
	completed := true
	if err := client.UpdateTaskContext(context.Background(), "task", UpdateTaskInput{Completed: &completed}); err == nil {
		t.Fatal("HTTP 200 API error hidden")
	}
	client.httpClient.Transport = refreshTransport(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.GetTaskContext(ctx, "task"); !errors.Is(err, context.Canceled) {
		t.Fatalf("get did not cancel: %v", err)
	}
	if err := client.UpdateTaskContext(ctx, "task", UpdateTaskInput{Completed: &completed}); !errors.Is(err, context.Canceled) {
		t.Fatalf("update did not cancel: %v", err)
	}
}

func TestConcurrentTokenRefresh(t *testing.T) {
	client := New(Config{AppID: "id", AppSecret: "secret"})
	client.httpClient.Transport = refreshTransport(func(r *http.Request) (*http.Response, error) {
		return response(`{"code":0,"tenant_access_token":"token","expire":7200}`), nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := client.tenantTokenContext(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}
