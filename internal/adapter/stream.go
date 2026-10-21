package adapter

import (
	"bytes"
	"encoding/json"
	"strings"
)

// StreamProcessor rewrites an SSE stream chunk by chunk.
//
// The difficulty is that a delta is not a unit of meaning: a `<think>` tag, or
// a single character, can be cut in half between two chunks. Everything
// buffered here exists so that a split tag is not emitted as literal text and
// a split character is not lost.
type StreamProcessor struct {
	mode    string
	pending string // tail held back because it might be the start of a tag
	inThink bool   // currently inside a <think> block
	emitted bool   // whether any content or reasoning reached the client
	// template is the envelope metadata (id/object/created/model) as last seen,
	// so chunks this package rebuilds keep looking like the upstream's.
	template chunkEnvelope
}

// NewStreamProcessor builds a processor for the given mode.
func NewStreamProcessor(mode string) *StreamProcessor {
	normalized := NormalizeMode(mode)
	if normalized == ModeContent {
		// Nothing to rewrite; leave the byte stream alone entirely.
		return &StreamProcessor{mode: normalized, emitted: true}
	}
	return &StreamProcessor{mode: normalized}
}

// Passthrough reports whether the processor returns input unchanged.
func (p *StreamProcessor) Passthrough() bool { return p.mode == ModeContent }

// HasContent reports whether any answer or reasoning was emitted. False after
// a stream that produced nothing is the signature of an upstream that accepted
// the request and then said nothing.
func (p *StreamProcessor) HasContent() bool { return p.emitted }

// ProcessChunk rewrites one SSE data payload.
//
// payload is the JSON after "data: ", or the literal "[DONE]". The return value
// is the list of payloads to send in its place, in order — usually one, but a
// chunk whose content contains both halves of a reasoning block becomes two.
// An empty result means the chunk was entirely buffered and nothing should be
// written for it yet.
func (p *StreamProcessor) ProcessChunk(payload []byte) [][]byte {
	trimmed := bytes.TrimSpace(payload)
	if p.mode == ModeContent || len(trimmed) == 0 || string(trimmed) == "[DONE]" {
		return [][]byte{payload}
	}

	var envelope chunkEnvelope
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return [][]byte{payload}
	}
	if len(envelope.Choices) == 0 {
		return [][]byte{payload}
	}
	// Remember the metadata before rebuilding anything: a rebuilt chunk that
	// dropped the id would look to some clients like the start of a new reply.
	p.template = envelope

	var out [][]byte
	changed := false
	for _, choice := range envelope.Choices {
		content := choice.Delta.Content
		if content == "" {
			// The opening delta (role only) and the closing one (finish_reason or
			// tool_calls) land here and are forwarded byte-for-byte, which is what
			// keeps tool_calls and usage intact.
			continue
		}
		segments := p.split(content)
		changed = true
		for _, segment := range segments {
			if segment.text == "" {
				continue
			}
			field := "content"
			if segment.reasoning {
				switch p.mode {
				case ModeStrip:
					// Dropped: the reasoning still counts as progress even though
					// nothing is sent for it, so a stream of pure reasoning does
					// not look like an empty start.
					p.emitted = true
					continue
				case ModeReasoningContent:
					field = "reasoning_content"
				}
			}
			p.emitted = true
			if chunk := p.encode(choice.Index, field, segment.text); chunk != nil {
				out = append(out, chunk)
			}
		}
	}

	if !changed {
		return [][]byte{payload}
	}
	return out
}

// Flush emits whatever is still buffered at end of stream.
//
// A half-received tag is released as content: dropping it would silently lose
// text, and an unterminated "<thi" is far more likely to be real content than
// the start of a tag the upstream never finished.
func (p *StreamProcessor) Flush() []byte {
	if p.pending == "" || p.mode == ModeContent {
		return nil
	}
	text := p.pending
	p.pending = ""
	reasoning := p.inThink
	p.inThink = false
	if reasoning && p.mode == ModeStrip {
		return nil
	}
	p.emitted = true
	field := "content"
	if reasoning && p.mode == ModeReasoningContent {
		field = "reasoning_content"
	}
	return p.encode(0, field, text)
}

// segment is a run of text belonging to either the reasoning or the answer.
type segment struct {
	text      string
	reasoning bool
}

const (
	openTag  = "<think>"
	closeTag = "</think>"
)

// split cuts a content delta into reasoning/answer runs, buffering any tail
// that could be the beginning of a tag.
func (p *StreamProcessor) split(text string) []segment {
	buffer := p.pending + text
	p.pending = ""
	var out []segment

	for buffer != "" {
		if p.inThink {
			if index := strings.Index(buffer, closeTag); index >= 0 {
				if index > 0 {
					out = append(out, segment{text: buffer[:index], reasoning: true})
				}
				buffer = buffer[index+len(closeTag):]
				p.inThink = false
				continue
			}
			keep := tagPrefixLen(buffer, closeTag)
			if emit := buffer[:len(buffer)-keep]; emit != "" {
				out = append(out, segment{text: emit, reasoning: true})
			}
			p.pending = buffer[len(buffer)-keep:]
			return out
		}

		if index := strings.Index(buffer, openTag); index >= 0 {
			if index > 0 {
				out = append(out, segment{text: buffer[:index]})
			}
			buffer = buffer[index+len(openTag):]
			p.inThink = true
			continue
		}
		keep := tagPrefixLen(buffer, openTag)
		if emit := buffer[:len(buffer)-keep]; emit != "" {
			out = append(out, segment{text: emit})
		}
		p.pending = buffer[len(buffer)-keep:]
		return out
	}
	return out
}

// tagPrefixLen returns how many trailing bytes of text could be the beginning
// of tag. Everything before them is safe to emit.
func tagPrefixLen(text, tag string) int {
	limit := len(tag) - 1
	if limit > len(text) {
		limit = len(text)
	}
	for size := limit; size > 0; size-- {
		if strings.HasSuffix(text, tag[:size]) {
			return size
		}
	}
	return 0
}

// chunkEnvelope is the part of an OpenAI stream chunk this package cares
// about. A chunk is only ever rebuilt when it carried plain text content, in
// which case these are the fields that matter.
type chunkEnvelope struct {
	ID      string          `json:"id,omitempty"`
	Object  string          `json:"object,omitempty"`
	Created int64           `json:"created,omitempty"`
	Model   string          `json:"model,omitempty"`
	System  json.RawMessage `json:"system_fingerprint,omitempty"`
	Usage   json.RawMessage `json:"usage,omitempty"`
	Choices []chunkChoice   `json:"choices"`
}

type chunkChoice struct {
	Index        int        `json:"index"`
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

type chunkDelta struct {
	Role string `json:"role,omitempty"`
	// Both text fields are omitempty: a reasoning chunk must not also carry an
	// empty `content`, or clients that render content unconditionally will show
	// a stray empty token for every reasoning delta.
	Content          string `json:"content,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// encode serialises a single chunk carrying one text field for one choice,
// reusing the metadata of the chunk it came from.
func (p *StreamProcessor) encode(index int, field, text string) []byte {
	object := p.template.Object
	if object == "" {
		object = "chat.completion.chunk"
	}
	choice := chunkChoice{Index: index}
	if field == "reasoning_content" {
		choice.Delta.ReasoningContent = text
	} else {
		choice.Delta.Content = text
	}
	out := chunkEnvelope{
		ID:      p.template.ID,
		Object:  object,
		Created: p.template.Created,
		Model:   p.template.Model,
		System:  p.template.System,
		Choices: []chunkChoice{choice},
	}
	encoded, err := json.Marshal(&out)
	if err != nil {
		return nil
	}
	return encoded
}
