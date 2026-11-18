package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

func disableAutomaticRetries(t *testing.T) {
	t.Helper()
	st := settings.Shared()
	st.NoAutomaticRetries = true
	if err := settings.Save(st); err != nil {
		t.Fatal(err)
	}
}

func TestNoAutomaticRetriesHTTP(t *testing.T) {
	for _, status := range []int{400, 401, 429, 500, 502, 503, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			for _, group := range []bool{false, true} {
				fresh(t)
				disableAutomaticRetries(t)
				said := fmt.Sprintf(`{"error":{"type":"api_error","message":"original error","code":%d}}`, status)
				a := &scripted{replies: []reply{{status, "", said}, {200, "", chatOK}}}
				b := &scripted{replies: []reply{{200, "", chatOK}}}
				scriptedOn(t, "a", provider.Chat, a)
				scriptedOn(t, "b", provider.Chat, b)
				if err := provider.SaveGroup(provider.Group{Name: "G", Members: []string{"a/m", "b/m"}, Routing: provider.Ordered}); err != nil {
					t.Fatal(err)
				}
				model := "a/m"
				if group {
					model = "group/g"
				}
				s := New()
				code, body := postAs(t, s, "", `{"model":"`+model+`","messages":[{"role":"user","content":"synthetic"}]}`)
				if code != status || body != said || a.n != 1 || b.n != 0 {
					t.Fatalf("group=%v: status=%d body=%s a=%d b=%d", group, code, body, a.n, b.n)
				}
				if r := s.trace.routes[len(s.trace.routes)-1]; len(r.Tries) != 1 || r.Tries[0].Status != status {
					t.Fatalf("tries: %+v", r.Tries)
				}
			}
		})
	}
}

func TestNoAutomaticRetriesStream(t *testing.T) {
	for _, translated := range []bool{false, true} {
		t.Run(fmt.Sprint(translated), func(t *testing.T) {
			fresh(t)
			disableAutomaticRetries(t)
			a := &scripted{replies: []reply{{200, "text/event-stream", cutShortStream}, {200, "text/event-stream", chatStreamSaid("must not retry")}}}
			b := &scripted{replies: []reply{{200, "text/event-stream", chatStreamSaid("must not fail over")}}}
			scriptedOn(t, "a", provider.Chat, a)
			scriptedOn(t, "b", provider.Chat, b)
			if err := provider.SaveGroup(provider.Group{Name: "G", Members: []string{"a/m", "b/m"}, Routing: provider.Ordered}); err != nil {
				t.Fatal(err)
			}
			path := "/v1/chat/completions"
			if translated {
				path = "/v1/messages"
			}
			w := httptest.NewRecorder()
			s := New()
			s.Handler().ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(`{"model":"group/g","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"synthetic"}]}`)))
			if a.n != 1 || b.n != 0 || !strings.Contains(w.Body.String(), "cut short") || strings.Contains(w.Body.String(), "must not") {
				t.Fatalf("%d %s a=%d b=%d", w.Code, w.Body, a.n, b.n)
			}
		})
	}
}

func TestNoAutomaticRetriesPlugin(t *testing.T) {
	var calls atomic.Int32
	const said = `{"type":"error","error":{"type":"api_error","message":"MITM carrier timed out"}}`
	pid := besideFake(t, "fakeco", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Should-Retry", "false")
		w.WriteHeader(504)
		io.WriteString(w, said)
	}))
	if _, err := plugin.APIKey(context.Background(), "fakeco", 0, nil, "k2", plugin.NewAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Providers(context.Background()); err != nil {
		t.Fatal(err)
	}
	disableAutomaticRetries(t)
	b := &scripted{replies: []reply{{200, "", chatOK}}}
	scriptedOn(t, "b", provider.Chat, b)
	if err := provider.SaveGroup(provider.Group{Name: "G", Members: []string{pid + "/fake-claude", "b/m"}, Routing: provider.Ordered}); err != nil {
		t.Fatal(err)
	}
	s := New()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"group/g","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"synthetic"}]}`)))
	if w.Code != 504 || w.Body.String() != said || calls.Load() != 1 || b.n != 0 || w.Header().Get("X-Should-Retry") != "false" {
		t.Fatalf("%d %s calls=%d fallback=%d retry=%q", w.Code, w.Body, calls.Load(), b.n, w.Header().Get("X-Should-Retry"))
	}
	if r := s.trace.routes[len(s.trace.routes)-1]; len(r.Tries) != 1 || r.Tries[0].Status != 504 {
		t.Fatalf("tries: %+v", r.Tries)
	}
}

func TestNoAutomaticRetriesInterruptedReply(t *testing.T) {
	fresh(t)
	disableAutomaticRetries(t)
	f := &cutChat{cuts: map[int][]string{0: {chatThink("kimi-k3", "thinking"), chatText("kimi-k3", "partial")}}, whole: []string{chatText("kimi-k3", "must not continue"), chatStop("kimi-k3")}}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.serve(t, w, r) }))
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{ID: "up", Name: "UP", Key: "k", Models: []string{"kimi-k3"}, Chat: up.URL + "/v1"}); err != nil {
		t.Fatal(err)
	}
	code, body := post(t, "/v1/messages", `{"model":"up/kimi-k3","max_tokens":32000,"stream":true,"messages":[{"role":"user","content":"synthetic"}]}`)
	if code != 200 || f.calls() != 1 || strings.Contains(body, "must not continue") {
		t.Fatalf("status=%d calls=%d body=%s", code, f.calls(), body)
	}
	if _, _, errs := messageStops(body); errs != 1 {
		t.Fatalf("missing stream error: %s", body)
	}
}

func TestNoAutomaticRetriesSuccessAndDefault(t *testing.T) {
	for _, off := range []bool{false, true} {
		fresh(t)
		if off {
			disableAutomaticRetries(t)
		}
		a := &scripted{replies: []reply{{503, "", `{"error":{"message":"busy"}}`}, {200, "", chatOK}}}
		scriptedOn(t, "a", provider.Chat, a)
		code, _ := postAs(t, New(), "", `{"model":"a/m","messages":[{"role":"user","content":"synthetic"}]}`)
		if off && (code != 503 || a.n != 1) || !off && (code != 200 || a.n != 2) {
			t.Fatalf("disabled=%v status=%d calls=%d", off, code, a.n)
		}
		code, body := postAs(t, New(), "", `{"model":"a/m","messages":[{"role":"user","content":"synthetic"}]}`)
		if code != 200 || !strings.Contains(body, "hello") {
			t.Fatalf("next user request: %d %s", code, body)
		}
	}
}
