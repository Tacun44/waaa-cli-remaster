package tui

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTranscribeDeepgram(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Token test-key" || r.Header.Get("Content-Type") != "audio/ogg" {
			t.Errorf("unexpected request method or headers: %s, %q, %q", r.Method, r.Header.Get("Authorization"), r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "ogg-data" {
			t.Errorf("audio body = %q", body)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"results":{"channels":[{"alternatives":[{"transcript":"  hola, cómo vas  "}]}]}}`))}, nil
	})}
	got, err := transcribeDeepgram(context.Background(), []byte("ogg-data"), "audio/ogg; codecs=opus", "test-key", "https://example.test/listen", client)
	if err != nil || got != "hola, cómo vas" {
		t.Fatalf("transcribeDeepgram = (%q, %v)", got, err)
	}
}
