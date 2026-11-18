package gateway

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestImageFinishMessageAndFailureUsage(t *testing.T) {
	const message = "Unable to show the generated image. The model could not generate the image based on the prompt provided. You will not be charged for this request. Try rephrasing the prompt."
	failure := fmt.Sprintf(`{"response":{"candidates":[{"content":{},"finishMessage":%q}],"usageMetadata":{"promptTokenCount":20,"totalTokenCount":20},"modelVersion":"gemini-3.1-flash-image"}}`, message)
	image := fmt.Sprintf(`{"response":{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":%q}}]}}]}}`, base64.StdEncoding.EncodeToString(pngBytes))
	stop := `{"response":{"candidates":[{"content":{"parts":[{"text":""}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":10}}}`
	for _, tc := range []struct {
		name, wire, code, reason string
		status                   int
	}{
		{"finish message without finish reason", "data: " + failure + "\n\n", "image_generation_failed", message, 502},
		{"empty response", "data: " + stop + "\n\n", "image_empty_response", "drew nothing", 502},
		{"image still succeeds", "data: " + image + "\n\ndata: " + stop + "\n\n", "", "", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			s, _ := antigravityGroupFor(t, []string{"test@example.test"}, "gemini-3.1-flash-image", func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, tc.wire)
			})
			req := httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(`{"model":"antigravity/gemini-3.1-flash-image","prompt":"A blue circle.","n":1,"response_format":"b64_json"}`))
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, req)
			if w.Code != tc.status {
				t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
			}
			rec := lastUsage(t)
			if rec.Input != 20 || rec.Status != tc.status {
				t.Fatalf("lost usage or status: %+v", rec)
			}
			if calls != 1 {
				t.Fatalf("unexpected extra requests: %d", calls)
			}
			if tc.status == 200 {
				var reply imagesAnswer
				if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
					t.Fatal(err)
				}
				if len(reply.Data) != 1 || reply.Data[0].B64 != base64.StdEncoding.EncodeToString(pngBytes) || rec.Error != "" {
					t.Fatalf("image reply changed: %s %+v", w.Body.String(), rec)
				}
			} else if !strings.Contains(w.Body.String(), tc.reason) || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) || !strings.Contains(rec.Error, tc.reason) || rec.ErrType != tc.code {
				t.Fatalf("failure lost: %s %+v", w.Body.String(), rec)
			}
		})
	}
}

func TestTextFinishMessageIsNotImageFailure(t *testing.T) {
	d := &codeAssistDecoder{}
	var events []Event
	err := d.decode(`{"response":{"candidates":[{"content":{},"finishReason":"STOP","finishMessage":"Completed"}],"modelVersion":"gemini-4-argon"}}`, func(ev Event) { events = append(events, ev) })
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Kind == KError {
			t.Fatalf("text reply became an image error: %+v", ev)
		}
	}
}
