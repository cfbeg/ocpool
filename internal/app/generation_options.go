package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

type GenerationOptions struct {
	Temperature     *float64           `json:"temperature,omitempty"`
	TopP            *float64           `json:"top_p,omitempty"`
	MaxOutputTokens *int               `json:"max_output_tokens,omitempty"`
	Stop            []string           `json:"-"`
	Reasoning       *ReasoningOptions  `json:"reasoning,omitempty"`
	Tools           *toolPlan          `json:"-"`
	ReasoningDelta  func(string) error `json:"-"`
	CacheAffinity   string             `json:"-"`
}

func (o GenerationOptions) validate() error {
	if o.Reasoning != nil {
		switch o.Reasoning.Effort {
		case "", "none", "minimal", "low", "medium", "high", "xhigh", "max":
		default:
			return errors.New("invalid reasoning effort")
		}
		switch o.Reasoning.Summary {
		case "", "auto", "concise", "detailed":
		default:
			return errors.New("invalid reasoning summary")
		}
	}
	if o.Temperature != nil && (math.IsNaN(*o.Temperature) || *o.Temperature < 0 || *o.Temperature > 2) {
		return errors.New("temperature must be between 0 and 2")
	}
	if o.TopP != nil && (math.IsNaN(*o.TopP) || *o.TopP < 0 || *o.TopP > 1) {
		return errors.New("top_p must be between 0 and 1")
	}
	if o.MaxOutputTokens != nil && (*o.MaxOutputTokens < 1 || *o.MaxOutputTokens > 1000000) {
		return errors.New("max_tokens must be between 1 and 1000000")
	}
	return nil
}
func parseStop(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var value string
	var list []string
	if json.Unmarshal(raw, &value) == nil {
		list = []string{value}
	} else if json.Unmarshal(raw, &list) != nil {
		return nil, errors.New("stop must be a string, an array of strings, or null")
	}
	if len(list) > 4 {
		return nil, errors.New("stop supports up to four sequences")
	}
	for _, s := range list {
		if s == "" || len(s) > 4096 {
			return nil, errors.New("stop sequences must contain 1 to 4096 bytes")
		}
	}
	return list, nil
}
func (o GenerationOptions) prepare(r *Runtime, session string) (func(), error) {
	if !validID(session) {
		return nil, errors.New("invalid upstream session id")
	}
	raw, e := json.Marshal(o)
	if e != nil {
		return nil, e
	}
	path := filepath.Join(r.ParamsDir, session+".json")
	if e = atomicWrite(path, raw); e != nil {
		return nil, e
	}
	return func() { _ = os.Remove(path); _ = os.Remove(filepath.Join(r.ParamsDir, session+".applied.json")) }, nil
}
func (o GenerationOptions) verify(r *Runtime, session string) error {
	if o.Temperature == nil && o.TopP == nil && o.MaxOutputTokens == nil && o.Reasoning == nil {
		return nil
	}
	raw, e := os.ReadFile(filepath.Join(r.ParamsDir, session+".applied.json"))
	if e != nil {
		return errors.New("OpenCode did not acknowledge the generation parameters")
	}
	var actual GenerationOptions
	if e = json.Unmarshal(raw, &actual); e != nil {
		return e
	}
	if o.Temperature != nil && (actual.Temperature == nil || *actual.Temperature != *o.Temperature) {
		return errors.New("OpenCode did not apply temperature")
	}
	if o.TopP != nil && (actual.TopP == nil || *actual.TopP != *o.TopP) {
		return errors.New("OpenCode did not apply top_p")
	}
	if o.MaxOutputTokens != nil && (actual.MaxOutputTokens == nil || *actual.MaxOutputTokens > *o.MaxOutputTokens) {
		return fmt.Errorf("OpenCode did not apply the output token limit")
	}
	if o.Reasoning != nil && (actual.Reasoning == nil || actual.Reasoning.Effort != o.Reasoning.Effort) {
		return errors.New("OpenCode did not acknowledge reasoning options")
	}
	return nil
}

// Hold back only a possible stop prefix, so delimiters cannot leak across SSE chunks.
type stopBuffer struct {
	patterns []string
	pending  string
	stopped  bool
}

func (s *stopBuffer) feed(delta string) (string, bool) {
	if s.stopped {
		return "", true
	}
	s.pending += delta
	first := -1
	for _, pattern := range s.patterns {
		if pos := strings.Index(s.pending, pattern); pos >= 0 && (first < 0 || pos < first) {
			first = pos
		}
	}
	if first >= 0 {
		out := s.pending[:first]
		s.pending = ""
		s.stopped = true
		return out, true
	}
	hold := 0
	for _, pattern := range s.patterns {
		for n := min(len(pattern)-1, len(s.pending)); n > hold; n-- {
			if strings.HasSuffix(s.pending, pattern[:n]) {
				hold = n
				break
			}
		}
	}
	out := s.pending[:len(s.pending)-hold]
	s.pending = s.pending[len(s.pending)-hold:]
	return out, false
}
func (s *stopBuffer) flush() string {
	if s.stopped {
		return ""
	}
	out := s.pending
	s.pending = ""
	return out
}
