package handler

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"teleagent2api/internal/adapter"
)

// wellFormedSSE re-serialises a stream so the assertions can talk about events
// instead of bytes: every event is one data line followed by exactly one blank
// line, and no event may be empty.
func eventsOf(t *testing.T, body string) []string {
	t.Helper()
	blocks := strings.Split(body, "\n\n")
	var events []string
	for _, block := range blocks {
		if strings.TrimSpace(block) == "" {
			// A trailing block after the last separator is normal.
			continue
		}
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, ":") {
				continue
			}
			if !strings.HasPrefix(line, "data:") {
				t.Errorf("unexpected non-data line %q in block %q", line, block)
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" {
				t.Errorf("empty data payload in block %q", block)
				continue
			}
			events = append(events, payload)
		}
	}
	return events
}

func runStream(t *testing.T, mode, upstream string) (body string, proc *adapter.StreamProcessor) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	proc = adapter.NewStreamProcessor(mode)
	streamCopy(rec, req, strings.NewReader(upstream), proc)
	return rec.Body.String(), proc
}

func TestStreamCopyRewritesAndFramesEvents(t *testing.T) {
	upstream := ": keep-alive\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","model":"chat-pro","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"<thi"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"nk>pondering</thi"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"nk>the answer"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	body, _ := runStream(t, adapter.ModeReasoningContent, upstream)
	events := eventsOf(t, body)

	var reasoning, content strings.Builder
	sawDone := false
	for _, event := range events {
		if event == "[DONE]" {
			sawDone = true
			continue
		}
		var envelope struct {
			Choices []struct {
				Delta map[string]any `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(event), &envelope); err != nil {
			t.Fatalf("event is not JSON: %v\n%s", err, event)
		}
		for _, choice := range envelope.Choices {
			if text, ok := choice.Delta["reasoning_content"].(string); ok {
				reasoning.WriteString(text)
			}
			if text, ok := choice.Delta["content"].(string); ok {
				content.WriteString(text)
			}
		}
	}

	if got := reasoning.String(); got != "pondering" {
		t.Errorf("reasoning_content = %q, want %q", got, "pondering")
	}
	if got := content.String(); got != "the answer" {
		t.Errorf("content = %q, want %q", got, "the answer")
	}
	if !sawDone {
		t.Error("[DONE] was not forwarded; many clients will hang")
	}
}

// TestStreamCopyBalancesBlankLines is the regression for the framing bug: a
// rewritten line writes its own terminator, so passing the upstream's blank
// line through as well would leave a double gap — and, with the buffered
// chunks, a bare event boundary the client reads as an empty message.
func TestStreamCopyBalancesBlankLines(t *testing.T) {
	upstream := `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"<think>"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"</think>"},"finish_reason":null}]}` + "\n\n" +
		"data: [DONE]\n\n"

	body, _ := runStream(t, adapter.ModeReasoningContent, upstream)
	if strings.Contains(body, "\n\n\n") {
		t.Errorf("stream has a triple newline (extra event boundary):\n%q", body)
	}
	events := eventsOf(t, body)
	if len(events) != 2 {
		t.Errorf("got %d events, want 2 (one reasoning delta, one [DONE]): %v", len(events), events)
	}
	// [DONE] must be the last event and must still be there.
	if len(events) > 0 && events[len(events)-1] != "[DONE]" {
		t.Errorf("last event = %q, want [DONE]", events[len(events)-1])
	}
}

func TestStreamCopyPassthroughLeavesBytesAlone(t *testing.T) {
	upstream := `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"<think>x</think>y"},"finish_reason":null}]}` + "\n\n" +
		"data: [DONE]\n\n"
	body, _ := runStream(t, adapter.ModeContent, upstream)
	if body != upstream {
		t.Errorf("mode=content altered the stream:\n got %q\nwant %q", body, upstream)
	}
}

func TestStreamCopyFlushesTrailingPartialTag(t *testing.T) {
	// No blank line after the final chunk, and the content ends mid-tag: the
	// text has to be released at EOF or it is silently lost.
	upstream := `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"answer <thi"},"finish_reason":null}]}`
	body, _ := runStream(t, adapter.ModeReasoningContent, upstream)
	if !strings.Contains(body, "answer") {
		t.Errorf("tail text was dropped: %q", body)
	}
	if strings.Contains(body, "<thi") == false && !strings.Contains(body, "\\u003cthi") {
		t.Errorf("buffered tag fragment was dropped rather than released: %q", body)
	}
}
