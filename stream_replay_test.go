package http_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	http "github.com/bogdanfinn/fhttp"
	http2 "github.com/bogdanfinn/fhttp/http2"
	tls "github.com/bogdanfinn/utls"
)

func TestStreamReplayPolicy(t *testing.T) {
	profiles := map[string]func() http.RoundTripper{
		"bundled": func() http.RoundTripper {
			return &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, ForceAttemptHTTP2: true}
		},
		"standalone": func() http.RoundTripper {
			return &http2.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
		},
	}
	for profile, transport := range profiles {
		for _, signal := range []string{"REFUSED_STREAM", "GOAWAY"} {
			for _, disabled := range []bool{false, true} {
				for _, method := range []string{http.MethodGet, http.MethodPost} {
					t.Run(fmt.Sprintf("%s/%s/disabled=%t/%s", profile, signal, disabled, method), func(t *testing.T) {
						peer := listenPeer(t, signal)
						client := &http.Client{Transport: transport(), Timeout: 3 * time.Second}
						t.Cleanup(client.CloseIdleConnections)
						body := ""
						if method == http.MethodPost {
							body = "payload"
						}
						send := func() (int, error) {
							request, err := http.NewRequest(method, "https://"+peer.listener.Addr().String()+"/probe", bytes.NewReader([]byte(body)))
							if err != nil {
								t.Fatal(err)
							}
							request.DisableRetries = disabled
							response, err := client.Do(request)
							if response == nil {
								return 0, err
							}
							_, readErr := io.Copy(io.Discard, response.Body)
							closeErr := response.Body.Close()
							if readErr != nil || closeErr != nil {
								t.Fatalf("response body: read=%v close=%v", readErr, closeErr)
							}
							if response.ProtoMajor != 2 {
								t.Fatalf("response protocol = %q; want HTTP/2", response.Proto)
							}
							return response.StatusCode, err
						}
						status, err := send()
						requests, connections := 2, 1
						if disabled {
							requests = 1
							var retry *http.RetryError
							if !errors.As(err, &retry) || !errors.Is(err, retry.Err) || !strings.Contains(retry.Err.Error(), signal) || status != 0 {
								t.Fatalf("Do status=%d error=%v; want RetryError retaining %s", status, err, signal)
							}
							if profile == "standalone" && signal == "REFUSED_STREAM" {
								var stream http2.StreamError
								if !errors.As(err, &stream) || stream.Code != http2.ErrCodeRefusedStream {
									t.Fatalf("original StreamError lost: %v", err)
								}
							}
						} else {
							if err != nil || status != http.StatusOK {
								t.Fatalf("default retry: status=%d error=%v", status, err)
							}
							if signal == "GOAWAY" {
								connections = 2
							}
						}
						peer.check(t, requests, connections, body)
						// Explicit follow-up preserves reuse after REFUSED_STREAM and
						// permits a fresh connection after GOAWAY without replay.
						status, err = send()
						if err != nil || status != http.StatusOK {
							t.Fatalf("explicit follow-up: status=%d error=%v", status, err)
						}
						if signal == "GOAWAY" {
							connections = 2
						}
						peer.check(t, requests+1, connections, body)
					})
				}
			}
		}
	}
}
