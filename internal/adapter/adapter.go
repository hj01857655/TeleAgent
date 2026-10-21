// Package adapter reshapes traffic between a generic OpenAI client and the
// TeleAgent upstream.
//
// The upstream is not a general-purpose OpenAI endpoint: it is the backend of
// the TeleAgent desktop app, which sends a narrow, fixed set of fields. A
// client like Claude Code sends considerably more (`metadata`, `top_k`,
// `system`, `thinking`, …), and the upstream rejects the whole request with
// "API 调用参数有误，请检查文档" when it meets a field it does not know. That
// message is the symptom this package exists to remove.
//
// Three unlike jobs live here, all of them response/request surgery:
//
//   - SanitizeRequest drops fields the upstream does not accept, and caps
//     max_tokens at what the model can actually emit.
//   - TransformNonStreaming and StreamProcessor move the model's
//     chain-of-thought out of `content`, where it would otherwise appear as
//     literal <think> tags in the answer.
//   - IsEmptyResponse recognises the upstream's occasional "200, but nothing
//     in it" reply so the caller can retry on another account.
package adapter

import (
	"encoding/json"
	"strings"
)

// requestFields is the set of top-level fields forwarded to the upstream.
//
// This is an allowlist rather than a denylist on purpose: the failure mode we
// are guarding against is "the upstream does not recognise this field", and a
// denylist can only ever enumerate the fields someone already hit. A field
// that some client needs can be added back at runtime through
// TELEAGENT_EXTRA_FIELDS, which is how this list stays short without becoming
// a dead end.
var requestFields = map[string]bool{
	"model":       true,
	"messages":    true,
	"stream":      true,
	"temperature": true,
	"top_p":       true,
	"max_tokens":  true,
	"tools":       true,
	"tool_choice": true,
}

// ModelLimits maps a model id to the largest max_tokens it accepts.
type ModelLimits map[string]int

// SanitizeRequest strips every top-level field outside the allowlist and caps
// max_tokens at the model's limit.
//
// A body that is not a JSON object is returned untouched: mangling input we
// cannot parse would turn a client bug into a gateway bug, and the upstream's
// own error is more useful than ours.
func SanitizeRequest(body []byte, extra []string, limits ModelLimits) []byte {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}

	cleaned := make(map[string]json.RawMessage, len(raw))
	for key, value := range raw {
		if requestFields[key] || containsFold(extra, key) {
			cleaned[key] = value
		}
	}

	if limit, ok := limits[modelName(cleaned["model"])]; ok && limit > 0 {
		if value, present := cleaned["max_tokens"]; present {
			var asked int
			if json.Unmarshal(value, &asked) == nil && asked > limit {
				capped, err := json.Marshal(limit)
				if err == nil {
					cleaned["max_tokens"] = capped
				}
			}
		}
	}

	out, err := json.Marshal(cleaned)
	if err != nil {
		return body
	}
	return out
}

func modelName(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var name string
	if json.Unmarshal(raw, &name) != nil {
		return ""
	}
	return name
}

func containsFold(list []string, want string) bool {
	for _, item := range list {
		if strings.EqualFold(strings.TrimSpace(item), want) {
			return true
		}
	}
	return false
}

// IsEmptyResponse reports whether a non-streaming reply carries no answer:
// no choices at all, or choices whose message has neither content nor
// reasoning.
//
// The upstream occasionally answers 200 with an empty choice — usually when it
// dropped the request on the floor rather than refusing it. Handing that to the
// client looks like a parse failure at best and a hang at worst, so the caller
// treats it as a failed attempt.
func IsEmptyResponse(body []byte) bool {
	var resp struct {
		Choices []struct {
			Message struct {
				Content          string            `json:"content"`
				ReasoningContent string            `json:"reasoning_content"`
				ToolCalls        []json.RawMessage `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &resp) != nil {
		// Unparseable is not the shape we are hunting for. Say "not empty" so the
		// raw bytes reach the client, where the error is at least diagnosable.
		return false
	}
	if len(resp.Choices) == 0 {
		return true
	}
	for _, choice := range resp.Choices {
		// A tool call is a complete answer even though `content` is null: that is
		// the shape of every function-calling reply, and counting it as empty
		// would retry a perfectly good turn on another account.
		if choice.Message.Content != "" || choice.Message.ReasoningContent != "" || len(choice.Message.ToolCalls) > 0 {
			return false
		}
	}
	return true
}

// Reasoning modes control where the model's chain-of-thought is delivered.
const (
	// ModeContent leaves the upstream reply exactly as it arrived, <think> tags
	// and all. Kept as an escape hatch for clients that render the tags.
	ModeContent = "content"
	// ModeReasoningContent splits the block into `reasoning_content` and leaves
	// `content` clean. Reasoning-aware clients (and gateways that map
	// reasoning_content onto Anthropic's `thinking`) show it live.
	ModeReasoningContent = "reasoning_content"
	// ModeVisible strips the tags but keeps the text in `content`, for clients
	// with no reasoning channel that still want to see progress.
	ModeVisible = "visible"
	// ModeStrip drops the reasoning entirely.
	ModeStrip = "strip"
)

// NormalizeMode maps an unrecognised or absent mode to ModeContent, which is
// the only mode that cannot make a reply worse.
func NormalizeMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ModeReasoningContent:
		return ModeReasoningContent
	case ModeVisible:
		return ModeVisible
	case ModeStrip:
		return ModeStrip
	default:
		return ModeContent
	}
}

const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

// TransformNonStreaming rewrites a complete chat-completion body so the
// reasoning is delivered according to mode.
//
// ModeContent returns the input untouched — not a no-op for elegance, but
// because every other mode has to re-serialise the JSON, and re-serialising is
// where field names get lost. The default mode should not touch the bytes.
func TransformNonStreaming(body []byte, mode string) []byte {
	if mode == ModeContent {
		return body
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return body
	}
	choicesRaw, ok := envelope["choices"]
	if !ok {
		return body
	}
	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(choicesRaw, &choices); err != nil {
		return body
	}

	changed := false
	for _, choice := range choices {
		messageRaw, ok := choice["message"]
		if !ok {
			continue
		}
		var message map[string]json.RawMessage
		if err := json.Unmarshal(messageRaw, &message); err != nil {
			continue
		}
		var content string
		if raw, ok := message["content"]; ok {
			if json.Unmarshal(raw, &content) != nil {
				continue
			}
		}
		reasoning, answer := splitThink(content)
		if reasoning == "" && !strings.Contains(content, thinkOpen) {
			// No reasoning block; leave the choice exactly as it was.
			continue
		}
		rewritten, err := json.Marshal(rewriteContent(message, reasoning, answer, mode))
		if err != nil {
			continue
		}
		choice["message"] = rewritten
		changed = true
	}

	if !changed {
		return body
	}
	// The choices were decoded into their own slice, so the envelope still holds
	// the original bytes. Writing them back is what makes the rewrite visible.
	reencoded, err := json.Marshal(choices)
	if err != nil {
		return body
	}
	envelope["choices"] = reencoded

	out, err := json.Marshal(envelope)
	if err != nil {
		return body
	}
	return out
}

// rewriteContent puts the two halves where the mode says they belong.
func rewriteContent(message map[string]json.RawMessage, reasoning, answer, mode string) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(message)+1)
	for key, value := range message {
		if key == "content" || key == "reasoning_content" {
			continue
		}
		out[key] = value
	}

	switch mode {
	case ModeReasoningContent:
		setString(out, "content", answer)
		if reasoning != "" {
			setString(out, "reasoning_content", reasoning)
		}
	case ModeStrip:
		setString(out, "content", answer)
	default: // ModeVisible
		setString(out, "content", reasoning+answer)
	}
	return out
}

func setString(target map[string]json.RawMessage, key, value string) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return
	}
	target[key] = encoded
}

// splitThink separates a <think>…</think> block from the answer that follows.
//
// The upstream puts the block at the very start of the content stream, so a
// single prefix match is enough; anything after the closing tag is the answer,
// including a second block, which is left alone rather than silently dropped.
func splitThink(content string) (reasoning, answer string) {
	open := strings.Index(content, thinkOpen)
	if open < 0 {
		return "", content
	}
	rest := content[open+len(thinkOpen):]
	closeIdx := strings.Index(rest, thinkClose)
	if closeIdx < 0 {
		// Unclosed block: treat everything as reasoning. The stream path handles
		// the same case by flushing at end-of-stream.
		return content[:open] + rest, content[:open]
	}
	return content[:open] + rest[:closeIdx], content[:open] + rest[closeIdx+len(thinkClose):]
}
