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

type streamOrder struct {
	name      string
	transport []string
	request   []string
	want      []string
}

func TestStreamReplayPolicy(t *testing.T) {
	profiles := map[string]func([]string) http.RoundTripper{
		"bundled": func(order []string) http.RoundTripper {
			return &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, ForceAttemptHTTP2: true, PseudoHeaderOrder: order}
		},
		"standalone": func(order []string) http.RoundTripper {
			return &http2.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, PseudoHeaderOrder: order}
		},
	}
	native := []string{":authority", ":method", ":path", ":scheme"}
	profileOrder := []string{":method", ":authority", ":scheme", ":path"}
	requestOrder := []string{":method", ":scheme", ":authority", ":path"}
	orders := []streamOrder{
		{name: "default", want: native},
		{name: "empty-transport", transport: []string{}, want: native},
		{name: "empty-request", request: []string{}, want: native},
		{name: "transport", transport: profileOrder, want: profileOrder},
		{name: "request", transport: profileOrder, request: requestOrder, want: requestOrder},
		{name: "empty-request-with-profile", transport: profileOrder, request: []string{}, want: profileOrder},
	}
	for profile, transport := range profiles {
		for _, signal := range []string{"REFUSED_STREAM", "GOAWAY"} {
			for _, disabled := range []bool{false, true} {
				for _, method := range []string{http.MethodGet, http.MethodPost} {
					for _, order := range orders {
						t.Run(fmt.Sprintf("%s/%s/disabled=%t/%s/%s", profile, signal, disabled, method, order.name), func(t *testing.T) {
							peer := listenPeer(t, streamScenario{signal: signal, method: method, order: order.want})
							client := &http.Client{Transport: transport(order.transport), Timeout: 3 * time.Second}
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
								if order.request != nil {
									request.Header[http.PHeaderOrderKey] = order.request
								}
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
}
