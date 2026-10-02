package serving

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAlphaSearchForwardsCodexRequestToCommandCodeProxy(t *testing.T) {
	const body = `{"id":"search-1","model":"gpt-5.6","commands":{"search_query":[{"q":"Cline"}]}}`
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/alpha/search" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer ccp-key" {
			t.Fatalf("authorization = %q", got)
		}
		got, _ := io.ReadAll(r.Body)
		if string(got) != body {
			t.Fatalf("request body = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"encrypted_output":null,"output":"ok","results":[]}`))
	}))
	defer search.Close()

	server, handler := newTestServer(t, "http://cline.invalid")
	settings := server.Config.Get()
	settings.SearchBaseURL = search.URL
	settings.SearchAPIKey = "ccp-key"
	if _, err := server.Config.Update(settings); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer gateway-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Body.String(); got != `{"encrypted_output":null,"output":"ok","results":[]}` {
		t.Fatalf("response body = %q", got)
	}
}

func TestAlphaSearchUsesIncomingGatewayKeyWhenDedicatedKeyIsUnset(t *testing.T) {
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gateway-key" {
			t.Fatalf("authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"output":"ok","results":[]}`))
	}))
	defer search.Close()

	server, handler := newTestServer(t, "http://cline.invalid")
	settings := server.Config.Get()
	settings.SearchBaseURL = search.URL
	settings.SearchAPIKey = ""
	if _, err := server.Config.Update(settings); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(`{"commands":{"search_query":[{"q":"Cline"}]}}`))
	request.Header.Set("Authorization", "Bearer gateway-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}
