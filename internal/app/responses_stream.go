package app

import (
	"net/http"
)

type responseSlot struct {
	kind, id, text string
	index          int
}
type responseStream struct {
	w                http.ResponseWriter
	writer           *sseStreamWriter
	enabled, started bool
	id, model        string
	created          int64
	sequence         int
	slots            []*responseSlot
}

func (s *responseStream) event(name string, body map[string]any) error {
	if !s.enabled {
		return nil
	}
	body["type"] = name
	body["sequence_number"] = s.sequence
	s.sequence++
	if s.writer != nil {
		return s.writer.event(name, body)
	}
	return sendSSE(s.w, name, body)
}
func (s *responseStream) start() error {
	if s.started {
		return nil
	}
	s.started = true
	if s.enabled && s.writer == nil {
		s.w.Header().Set("Content-Type", "text/event-stream")
		s.w.Header().Set("Cache-Control", "no-cache")
		s.w.Header().Set("X-Accel-Buffering", "no")
	}
	return s.event("response.created", map[string]any{"response": map[string]any{"id": s.id, "object": "response", "created_at": s.created, "status": "in_progress", "model": s.model, "output": []any{}}})
}
func (s *responseStream) slot(kind string) (*responseSlot, error) {
	for _, slot := range s.slots {
		if slot.kind == kind {
			return slot, nil
		}
	}
	if e := s.start(); e != nil {
		return nil, e
	}
	prefix := "msg"
	if kind == "reasoning" {
		prefix = "rs"
	}
	slot := &responseSlot{kind: kind, id: newID(prefix), index: len(s.slots)}
	s.slots = append(s.slots, slot)
	item := map[string]any{"id": slot.id, "type": kind, "status": "in_progress"}
	if kind == "message" {
		item["role"] = "assistant"
		item["content"] = []any{}
	} else {
		item["summary"] = []any{}
	}
	if e := s.event("response.output_item.added", map[string]any{"output_index": slot.index, "item": item}); e != nil {
		return nil, e
	}
	name, index, partType := "response.content_part.added", "content_index", "output_text"
	if kind == "reasoning" {
		name, index, partType = "response.reasoning_summary_part.added", "summary_index", "summary_text"
	}
	return slot, s.event(name, map[string]any{"output_index": slot.index, "item_id": slot.id, index: 0, "part": map[string]any{"type": partType, "text": "", "annotations": []any{}}})
}
func (s *responseStream) delta(kind, text string) error {
	slot, e := s.slot(kind)
	if e != nil {
		return e
	}
	slot.text += text
	name, index := "response.output_text.delta", "content_index"
	if kind == "reasoning" {
		name, index = "response.reasoning_summary_text.delta", "summary_index"
	}
	return s.event(name, map[string]any{"output_index": slot.index, "item_id": slot.id, index: 0, "delta": text})
}
func (s *responseStream) finish(result Result) ([]any, error) {
	if e := s.start(); e != nil {
		return nil, e
	}
	if result.Reasoning != "" {
		if _, e := s.slot("reasoning"); e != nil {
			return nil, e
		}
	}
	if result.Text != "" || len(result.ToolCalls) == 0 {
		if _, e := s.slot("message"); e != nil {
			return nil, e
		}
	}
	output := []any{}
	for _, slot := range s.slots {
		text := result.Text
		partType, partKey, index := "output_text", "content", "content_index"
		textEvent, partEvent := "response.output_text.done", "response.content_part.done"
		if slot.kind == "reasoning" {
			text = result.Reasoning
			partType, partKey, index = "summary_text", "summary", "summary_index"
			textEvent, partEvent = "response.reasoning_summary_text.done", "response.reasoning_summary_part.done"
		}
		part := map[string]any{"type": partType, "text": text}
		if slot.kind == "message" {
			part["annotations"] = []any{}
		}
		item := map[string]any{"id": slot.id, "type": slot.kind, "status": "completed", partKey: []any{part}}
		if slot.kind == "message" {
			item["role"] = "assistant"
			if result.FinishReason == "length" {
				item["status"] = "incomplete"
			}
		}
		if e := s.event(textEvent, map[string]any{"output_index": slot.index, "item_id": slot.id, index: 0, "text": text}); e != nil {
			return nil, e
		}
		if e := s.event(partEvent, map[string]any{"output_index": slot.index, "item_id": slot.id, index: 0, "part": part}); e != nil {
			return nil, e
		}
		if e := s.event("response.output_item.done", map[string]any{"output_index": slot.index, "item": item}); e != nil {
			return nil, e
		}
		output = append(output, item)
	}
	for _, call := range result.ToolCalls {
		index := len(output)
		id := newID("fc")
		item := map[string]any{"id": id, "type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": "", "status": "in_progress"}
		if e := s.event("response.output_item.added", map[string]any{"output_index": index, "item": item}); e != nil {
			return nil, e
		}
		if e := s.event("response.function_call_arguments.delta", map[string]any{"output_index": index, "item_id": id, "delta": call.Function.Arguments}); e != nil {
			return nil, e
		}
		if e := s.event("response.function_call_arguments.done", map[string]any{"output_index": index, "item_id": id, "arguments": call.Function.Arguments}); e != nil {
			return nil, e
		}
		item["arguments"] = call.Function.Arguments
		item["status"] = "completed"
		if e := s.event("response.output_item.done", map[string]any{"output_index": index, "item": item}); e != nil {
			return nil, e
		}
		output = append(output, item)
	}
	return output, nil
}
