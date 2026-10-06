package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	api "github.com/acksell/opencode-go-sdk/sdk"
	"github.com/acksell/opencode-go-sdk/sdk/client"
	"github.com/acksell/opencode-go-sdk/sdk/option"
)

type connection struct {
	URL           string `json:"url"`
	Authorization string `json:"authorization"`
	Directory     string `json:"directory"`
}

func run() error {
	config := flag.String("connection", "", "Path to private runtime connection JSON")
	model := flag.String("model", "", "Explicit provider/model to probe")
	prompt := flag.String("prompt", "", "Optional single test prompt")
	flag.Parse()
	if *config == "" {
		return errors.New("--connection is required")
	}
	raw, err := os.ReadFile(*config)
	if err != nil {
		return err
	}
	var cfg connection
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	c := client.NewClient(option.WithBaseURL(cfg.URL), option.WithHTTPHeader(http.Header{"Authorization": {cfg.Authorization}}), option.WithMaxAttempts(1))
	health, err := c.Global.Health(ctx)
	if err != nil {
		return fmt.Errorf("health: %w", err)
	}
	catalog, err := c.Config.Providers(ctx, &api.ConfigProvidersRequest{Directory: &cfg.Directory})
	if err != nil {
		return fmt.Errorf("models: %w", err)
	}
	free := make([]string, 0)
	for _, p := range catalog.Providers {
		if p.ID != "opencode" {
			continue
		}
		for id, m := range p.Models {
			if m.Cost != nil && m.Cost.Input == 0 && m.Cost.Output == 0 {
				free = append(free, p.ID+"/"+id)
			}
		}
	}
	sort.Strings(free)
	result := map[string]any{"health": health, "zero_cost_catalog_models": free}
	if *prompt == "" {
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	parts := strings.SplitN(*model, "/", 2)
	if len(parts) != 2 {
		return errors.New("--model provider/model is required with --prompt")
	}
	allowed := false
	for _, candidate := range free {
		if candidate == *model {
			allowed = true
		}
	}
	if !allowed {
		return errors.New("requested model is not listed as zero-cost by this OpenCode runtime")
	}
	streamCtx, stopStream := context.WithCancel(ctx)
	defer stopStream()
	events, err := c.Global.Event(streamCtx)
	if err != nil {
		return fmt.Errorf("events: %w", err)
	}
	defer events.Close()
	// Receiving the initial event establishes the subscription before sending the prompt.
	if _, err := events.Recv(); err != nil {
		return fmt.Errorf("event handshake: %w", err)
	}
	result["sse_connected"] = true
	go func() {
		for {
			if _, err := events.Recv(); err != nil {
				return
			}
		}
	}()
	session, err := c.Session.Create(ctx, &api.SessionCreateRequest{Directory: &cfg.Directory})
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_, _ = c.Session.Delete(cleanup, &api.SessionDeleteRequest{SessionID: session.ID, Directory: &cfg.Directory})
	}()
	response, err := c.Session.Prompt(ctx, &api.SessionPromptRequest{
		SessionID: session.ID, Directory: &cfg.Directory,
		Model: &api.SessionPromptRequestModel{ProviderID: parts[0], ModelID: parts[1]},
		Parts: []*api.SessionPromptRequestPartsItem{{Type: "text", Text: &api.TextPartInput{Text: *prompt}}},
	})
	if err != nil {
		return fmt.Errorf("prompt: %w", err)
	}
	if response.Info != nil && response.Info.Error != nil {
		upstream := response.Info.Error
		message := ""
		if upstream.APIError != nil && upstream.APIError.Data != nil {
			message = upstream.APIError.Data.Message
		}
		if upstream.ProviderAuthError != nil && upstream.ProviderAuthError.Data != nil {
			message = upstream.ProviderAuthError.Data.Message
		}
		if upstream.UnknownError != nil && upstream.UnknownError.Data != nil {
			message = upstream.UnknownError.Data.Message
		}
		if raw, marshalErr := json.MarshalIndent(map[string]any{"name": upstream.Name, "message": message}, "", "  "); marshalErr == nil {
			_ = os.WriteFile(filepath.Join(filepath.Dir(*config), "probe-error.json"), raw, 0600)
		}
		return fmt.Errorf("upstream %s: %s", upstream.Name, message)
	}
	var text strings.Builder
	for _, p := range response.Parts {
		if p.Text != nil {
			text.WriteString(p.Text.Text)
		}
	}
	result["model"] = *model
	result["text"] = text.String()
	if text.Len() == 0 {
		return errors.New("empty model response")
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
