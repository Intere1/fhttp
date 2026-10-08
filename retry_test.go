package http_test

import (
	"context"
	"errors"
	"io"
	"net"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

func TestHTTP1RetryAdmission(t *testing.T) {
	for _, method := range []string{"GET", "POST"} {
		for _, disabled := range []bool{false, true} {
			name := "default"
			if disabled {
				name = "disabled"
			}
			t.Run(method+"/"+name, func(t *testing.T) {
				var attempts, received, connections atomic.Int32
				server := httptest.NewUnstartedServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					received.Add(int32(len(body)))
					if attempts.Add(1) == 2 {
						conn, _, err := w.(stdhttp.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						if err := conn.Close(); err != nil {
							t.Error(err)
						}
						return
					}
					if _, err := io.WriteString(w, "ok"); err != nil {
						t.Error(err)
					}
				}))
				server.Config.ConnState = func(_ net.Conn, state stdhttp.ConnState) {
					if state == stdhttp.StateNew {
						connections.Add(1)
					}
				}
				server.Start()
				defer server.Close()
				client := &fhttp.Client{Transport: &fhttp.Transport{}, Timeout: 3 * time.Second}
				defer client.CloseIdleConnections()
				var requestErr error
				for n := 0; n < 2; n++ {
					var body io.Reader
					if method == "POST" {
						body = strings.NewReader("payload")
					}
					req, err := fhttp.NewRequest(method, server.URL, body)
					if err != nil {
						t.Fatal(err)
					}
					// HTTP/1 only retries POST when the caller asserts idempotency.
					if method == "POST" {
						req.Header.Set("Idempotency-Key", "test")
					}
					req.DisableRetries = disabled
					resp, err := client.Do(req)
					requestErr = err
					if resp != nil {
						data, readErr := io.ReadAll(resp.Body)
						closeErr := resp.Body.Close()
						if readErr != nil || closeErr != nil || resp.StatusCode != 200 || string(data) != "ok" {
							t.Fatalf("response status=%d body=%q read=%v close=%v", resp.StatusCode, data, readErr, closeErr)
						}
					}
					if n == 0 && requestErr != nil {
						t.Fatalf("connection warm-up: %v", requestErr)
					}
				}
				wantAttempts, wantConnections := int32(3), int32(2)
				if disabled {
					wantAttempts, wantConnections = 2, 1
				}
				wantBytes := int32(0)
				if method == "POST" {
					wantBytes = 7 * wantAttempts
				}
				if attempts.Load() != wantAttempts || received.Load() != wantBytes || connections.Load() != wantConnections {
					t.Fatalf("requests=%d body bytes=%d connections=%d; want %d/%d/%d", attempts.Load(), received.Load(), connections.Load(), wantAttempts, wantBytes, wantConnections)
				}
				var retry *fhttp.RetryError
				if disabled {
					if !errors.As(requestErr, &retry) || retry.Err == nil || !errors.Is(requestErr, retry.Err) {
						t.Fatalf("retry cause lost: %v", requestErr)
					}
				} else if requestErr != nil {
					t.Fatalf("default retry failed: %v", requestErr)
				}
			})
		}
	}
}

func TestRetryPolicyPropagation(t *testing.T) {
	for _, copyRequest := range []string{"original", "Clone", "WithContext"} {
		for _, disabled := range []bool{false, true} {
			name := "default"
			if disabled {
				name = "disabled"
			}
			t.Run(copyRequest+"/"+name, func(t *testing.T) {
				var attempts, failures atomic.Int32
				server := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
					attempts.Add(1)
					if r.URL.Path == "/redirect" {
						stdhttp.Redirect(w, r, "/fail", stdhttp.StatusFound)
						return
					}
					if failures.Add(1) == 1 {
						conn, _, err := w.(stdhttp.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						if err := conn.Close(); err != nil {
							t.Error(err)
						}
						return
					}
					if _, err := io.WriteString(w, "ok"); err != nil {
						t.Error(err)
					}
				}))
				defer server.Close()
				client := &fhttp.Client{Transport: &fhttp.Transport{}, Timeout: 3 * time.Second}
				defer client.CloseIdleConnections()
				req, err := fhttp.NewRequest("GET", server.URL+"/redirect", nil)
				if err != nil {
					t.Fatal(err)
				}
				req.DisableRetries = disabled
				switch copyRequest {
				case "Clone":
					req = req.Clone(context.Background())
				case "WithContext":
					req = req.WithContext(context.Background())
				}
				resp, requestErr := client.Do(req)
				if resp != nil {
					data, readErr := io.ReadAll(resp.Body)
					closeErr := resp.Body.Close()
					if readErr != nil || closeErr != nil || resp.StatusCode != 200 || string(data) != "ok" {
						t.Fatalf("response status=%d body=%q read=%v close=%v", resp.StatusCode, data, readErr, closeErr)
					}
				}
				wantAttempts, wantFailures := int32(3), int32(2)
				if disabled {
					wantAttempts, wantFailures = 2, 1
				}
				if attempts.Load() != wantAttempts || failures.Load() != wantFailures {
					t.Fatalf("redirect lost policy: requests=%d fail-route requests=%d; want %d/%d", attempts.Load(), failures.Load(), wantAttempts, wantFailures)
				}
				var retry *fhttp.RetryError
				if disabled && !errors.As(requestErr, &retry) {
					t.Fatalf("redirect lost retry cause: %v", requestErr)
				}
				if !disabled && requestErr != nil {
					t.Fatalf("default redirect retry failed: %v", requestErr)
				}
			})
		}
	}
}
