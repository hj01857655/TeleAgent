package adapter

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSanitizeStripsUnknownFields(t *testing.T) {
	body := []byte(`{
		"model": "chat-pro",
		"messages": [{"role": "user", "content": "hi"}],
		"max_tokens": 100,
		"metadata": {"user_id": "abc"},
		"top_k": 40,
		"thinking": {"type": "enabled"}
	}`)
	out := SanitizeRequest(body, nil, nil)

	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	for _, drop := range []string{"metadata", "top_k", "thinking"} {
		if _, ok := got[drop]; ok {
			t.Errorf("%s should have been stripped", drop)
		}
	}
	for _, keep := range []string{"model", "messages", "max_tokens"} {
		if _, ok := got[keep]; !ok {
			t.Errorf("%s should have been kept", keep)
		}
	}
}

func TestSanitizeCapsMaxTokens(t *testing.T) {
	body := []byte(`{"model":"chat-nano","messages":[],"max_tokens":200000}`)
	out := SanitizeRequest(body, nil, ModelLimits{"chat-nano": 16384})

	var got struct {
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if got.MaxTokens != 16384 {
		t.Errorf("max_tokens = %d, want 16384", got.MaxTokens)
	}
}

func TestSanitizeLeavesSmallerMaxTokensAlone(t *testing.T) {
	body := []byte(`{"model":"chat-nano","max_tokens":100}`)
	out := SanitizeRequest(body, nil, ModelLimits{"chat-nano": 16384})
	var got struct {
		MaxTokens int `json:"max_tokens"`
	}
	json.Unmarshal(out, &got)
	if got.MaxTokens != 100 {
		t.Errorf("max_tokens = %d, want 100 (only oversized values are capped)", got.MaxTokens)
	}
}

func TestSanitizeHonoursExtraFields(t *testing.T) {
	body := []byte(`{"model":"m","messages":[],"reasoning_effort":"high"}`)
	out := SanitizeRequest(body, []string{"reasoning_effort"}, nil)
	if !strings.Contains(string(out), "reasoning_effort") {
		t.Errorf("extra field was stripped: %s", out)
	}
}

func TestSanitizeReportsDroppedFields(t *testing.T) {
	// The report is what makes an upstream "parameters are wrong" error
	// actionable, so the names — and their order — matter.
	body := []byte(`{"model":"m","messages":[],"top_k":5,"metadata":{},"thinking":{}, "anz":1}`)
	_, dropped := SanitizeRequestReport(body, nil, nil)
	want := []string{"anz", "metadata", "thinking", "top_k"}
	if len(dropped) != len(want) {
		t.Fatalf("dropped = %v, want %v", dropped, want)
	}
	for i := range want {
		if dropped[i] != want[i] {
			t.Errorf("dropped[%d] = %q, want %q (must be sorted)", i, dropped[i], want[i])
		}
	}
}

func TestSanitizeReportIsEmptyWhenNothingDropped(t *testing.T) {
	body := []byte(`{"model":"m","messages":[]}`)
	out, dropped := SanitizeRequestReport(body, nil, nil)
	if len(dropped) != 0 {
		t.Errorf("dropped = %v, want none", dropped)
	}
	if len(out) == 0 {
		t.Error("output is empty")
	}
}

func TestSanitizePassesThroughUnparseableBody(t *testing.T) {
	body := []byte(`not json at all`)
	out := SanitizeRequest(body, nil, nil)
	if string(out) != string(body) {
		t.Errorf("unparseable body was modified: %q", out)
	}
}

func TestIsEmptyResponse(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"no choices", `{"choices":[]}`, true},
		{"missing choices", `{"id":"x"}`, true},
		{"empty content", `{"choices":[{"message":{"content":""}}]}`, true},
		{"reasoning only", `{"choices":[{"message":{"reasoning_content":"thinking"}}]}`, false},
		{"tool call", `{"choices":[{"message":{"content":null,"tool_calls":[{"id":"c1","type":"function"}]}}]}`, false},
		{"has answer", `{"choices":[{"message":{"content":"hi"}}]}`, false},
		{"not json", `{`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsEmptyResponse([]byte(tc.body)); got != tc.want {
				t.Errorf("IsEmptyResponse = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTransformNonStreamingReasoningContent(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"<think>let me think</think>the answer"}}]}`)
	out := TransformNonStreaming(body, ModeReasoningContent)

	var got struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Role             string `json:"role"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	msg := got.Choices[0].Message
	if msg.Content != "the answer" {
		t.Errorf("content = %q, want %q", msg.Content, "the answer")
	}
	if msg.ReasoningContent != "let me think" {
		t.Errorf("reasoning_content = %q, want %q", msg.ReasoningContent, "let me think")
	}
	// Fields the adapter does not care about must survive the rewrite.
	if msg.Role != "assistant" {
		t.Errorf("role was lost: %q", msg.Role)
	}
}

func TestTransformNonStreamingStrip(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"<think>hidden</think>shown"}}]}`)
	out := TransformNonStreaming(body, ModeStrip)
	var got struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	json.Unmarshal(out, &got)
	if got.Choices[0].Message.Content != "shown" {
		t.Errorf("content = %q, want %q", got.Choices[0].Message.Content, "shown")
	}
	if got.Choices[0].Message.ReasoningContent != "" {
		t.Errorf("reasoning_content should be absent, got %q", got.Choices[0].Message.ReasoningContent)
	}
}

func TestTransformNonStreamingVisible(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"<think>thought</think>answer"}}]}`)
	out := TransformNonStreaming(body, ModeVisible)
	var got struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	json.Unmarshal(out, &got)
	if got.Choices[0].Message.Content != "thoughtanswer" {
		t.Errorf("content = %q, want %q", got.Choices[0].Message.Content, "thoughtanswer")
	}
}

func TestTransformNonStreamingContentModeIsByteIdentical(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"<think>x</think>y"}}]}`)
	out := TransformNonStreaming(body, ModeContent)
	if string(out) != string(body) {
		t.Errorf("mode=content altered the body:\n got %s\nwant %s", out, body)
	}
}

func TestTransformNonStreamingWithoutThinkIsUnchanged(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"plain answer"}}]}`)
	out := TransformNonStreaming(body, ModeReasoningContent)
	if string(out) != string(body) {
		t.Errorf("a reply with no reasoning was rewritten: %s", out)
	}
}

// ---------------------------------------------------------------- streaming

// feed pushes text through a processor and collects every emitted chunk.
func feed(t *testing.T, mode string, deltas ...string) [][]byte {
	t.Helper()
	p := NewStreamProcessor(mode)
	var out [][]byte
	for _, delta := range deltas {
		payload, err := json.Marshal(map[string]any{
			"id":     "chatcmpl-1",
			"object": "chat.completion.chunk",
			"model":  "chat-pro",
			"choices": []map[string]any{
				{"index": 0, "delta": map[string]any{"content": delta}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p.ProcessChunk(payload)...)
	}
	if tail := p.Flush(); tail != nil {
		out = append(out, tail)
	}
	return out
}

// collect returns every text sent for field, in order.
func collect(t *testing.T, chunks [][]byte, field string) string {
	t.Helper()
	var b strings.Builder
	for _, chunk := range chunks {
		var envelope struct {
			Choices []struct {
				Delta map[string]any `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(chunk, &envelope); err != nil {
			t.Fatalf("emitted chunk is not JSON: %v\n%s", err, chunk)
		}
		for _, choice := range envelope.Choices {
			if text, ok := choice.Delta[field].(string); ok {
				b.WriteString(text)
			}
		}
	}
	return b.String()
}

func TestStreamSplitsReasoningAcrossChunks(t *testing.T) {
	// The tag is split mid-word, which is what a real tokenizer does.
	chunks := feed(t, ModeReasoningContent, "<thi", "nk>thinking", " hard</thi", "nk>", "the answer")
	if got := collect(t, chunks, "reasoning_content"); got != "thinking hard" {
		t.Errorf("reasoning_content = %q, want %q", got, "thinking hard")
	}
	if got := collect(t, chunks, "content"); got != "the answer" {
		t.Errorf("content = %q, want %q", got, "the answer")
	}
}

func TestStreamDoesNotLeakTagFragments(t *testing.T) {
	chunks := feed(t, ModeReasoningContent, "<", "thi", "nk>", "x", "</", "think>", "y")
	for _, chunk := range chunks {
		var envelope struct {
			Choices []struct {
				Delta map[string]any `json:"delta"`
			} `json:"choices"`
		}
		json.Unmarshal(chunk, &envelope)
		for _, choice := range envelope.Choices {
			for _, field := range []string{"content", "reasoning_content"} {
				if text, ok := choice.Delta[field].(string); ok && strings.Contains(text, "thi") {
					t.Errorf("tag fragment %q leaked into %s", text, field)
				}
			}
		}
	}
	if got := collect(t, chunks, "content"); got != "y" {
		t.Errorf("content = %q, want %q", got, "y")
	}
}

func TestStreamStripModeEmitsNoReasoning(t *testing.T) {
	chunks := feed(t, ModeStrip, "<think>hidden</think>", "shown")
	if got := collect(t, chunks, "reasoning_content"); got != "" {
		t.Errorf("strip mode emitted reasoning: %q", got)
	}
	if got := collect(t, chunks, "content"); got != "shown" {
		t.Errorf("content = %q, want %q", got, "shown")
	}
}

func TestStreamVisibleModeKeepsReasoningInContent(t *testing.T) {
	chunks := feed(t, ModeVisible, "<think>thought</think>answer")
	if got := collect(t, chunks, "content"); got != "thoughtanswer" {
		t.Errorf("content = %q, want %q", got, "thoughtanswer")
	}
	if got := collect(t, chunks, "reasoning_content"); got != "" {
		t.Errorf("visible mode used reasoning_content: %q", got)
	}
}

func TestStreamReportsContentEmitted(t *testing.T) {
	p := NewStreamProcessor(ModeReasoningContent)
	if p.HasContent() {
		t.Error("a fresh processor claims to have emitted content")
	}
	payload, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": "x"}}},
	})
	p.ProcessChunk(payload)
	if !p.HasContent() {
		t.Error("content was emitted but HasContent() is false")
	}
}

func TestStreamContentModeIsPassthrough(t *testing.T) {
	p := NewStreamProcessor(ModeContent)
	if !p.Passthrough() {
		t.Error("mode=content should report passthrough")
	}
	payload := []byte(`{"choices":[{"delta":{"content":"<think>x</think>y"}}]}`)
	out := p.ProcessChunk(payload)
	if len(out) != 1 || string(out[0]) != string(payload) {
		t.Errorf("mode=content rewrote the chunk: %s", out)
	}
}

func TestStreamUnclosedTagFlushesAsContent(t *testing.T) {
	// A tag that never closes is almost certainly text the model meant to say.
	chunks := feed(t, ModeReasoningContent, "answer <thi")
	all := collect(t, chunks, "content") + collect(t, chunks, "reasoning_content")
	if !strings.Contains(all, "<thi") {
		t.Errorf("unterminated tag was dropped; emitted %q", all)
	}
}

func TestStreamKeepsMetadata(t *testing.T) {
	chunks := feed(t, ModeReasoningContent, "hi")
	if len(chunks) == 0 {
		t.Fatal("no chunks emitted")
	}
	var envelope struct {
		ID    string `json:"id"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(chunks[0], &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.ID != "chatcmpl-1" || envelope.Model != "chat-pro" {
		t.Errorf("chunk lost metadata: id=%q model=%q", envelope.ID, envelope.Model)
	}
}

func TestStreamKeepsChoiceIndex(t *testing.T) {
	// A request with n>1 must not have every choice's text folded into index 0.
	p := NewStreamProcessor(ModeReasoningContent)
	payload, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-1",
		"choices": []map[string]any{
			{"index": 0, "delta": map[string]any{"content": "first"}},
			{"index": 1, "delta": map[string]any{"content": "second"}},
		},
	})
	chunks := p.ProcessChunk(payload)
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	for i, chunk := range chunks {
		var envelope struct {
			Choices []struct {
				Index int `json:"index"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(chunk, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Choices[0].Index != i {
			t.Errorf("chunk %d carries index %d, want %d", i, envelope.Choices[0].Index, i)
		}
	}
}

func TestStreamForwardsRoleDelta(t *testing.T) {
	// The role delta and the finish delta carry no content, so they must reach
	// the client byte-for-byte; clients key the start of a message off the role.
	p := NewStreamProcessor(ModeReasoningContent)
	role := []byte(`{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`)
	out := p.ProcessChunk(role)
	if len(out) != 1 || string(out[0]) != string(role) {
		t.Errorf("role delta was rewritten: %s", out)
	}

	finish := []byte(`{"id":"c1","choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop"}]}`)
	out = p.ProcessChunk(finish)
	if len(out) != 1 || string(out[0]) != string(finish) {
		t.Errorf("finish delta was rewritten: %s", out)
	}
}

func TestStreamBuffersTagOnlyChunkSilently(t *testing.T) {
	// A chunk that is exactly the opening tag produces no text; emitting an
	// empty content event instead would show up as a stray empty token.
	chunks := feed(t, ModeReasoningContent, "<think>", "x", "</think>", "y")
	if got := collect(t, chunks, "reasoning_content"); got != "x" {
		t.Errorf("reasoning_content = %q, want %q", got, "x")
	}
	if got := collect(t, chunks, "content"); got != "y" {
		t.Errorf("content = %q, want %q", got, "y")
	}
	for _, chunk := range chunks {
		var envelope struct {
			Choices []struct {
				Delta map[string]any `json:"delta"`
			} `json:"choices"`
		}
		json.Unmarshal(chunk, &envelope)
		for _, choice := range envelope.Choices {
			for _, field := range []string{"content", "reasoning_content"} {
				if text, ok := choice.Delta[field].(string); ok && text == "" {
					t.Errorf("emitted an empty %s event", field)
				}
			}
		}
	}
}
