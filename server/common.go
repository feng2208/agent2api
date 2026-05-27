package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"acp-gateway/acp"
	"acp-gateway/config"
)

type contextKey string

const APIKeyContextKey contextKey = "api_key"

func newUUID() string {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// ImageURLConfig represents the OpenAI image_url format.
type ImageURLConfig struct {
	URL string `json:"url"`
}

// ContentPart represents a single content part in a multi-modal message (OpenAI format).
type ContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *ImageURLConfig `json:"image_url,omitempty"`
}

// MessageContent supports both string and array content formats via custom JSON unmarshaling.
type MessageContent struct {
	IsString bool
	Text     string
	Parts    []ContentPart
}

func (m *MessageContent) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		m.IsString = true
		return json.Unmarshal(data, &m.Text)
	}
	m.IsString = false
	return json.Unmarshal(data, &m.Parts)
}

// processImageURL handles both data: URIs and remote HTTP image URLs,
// returning the mime type and base64-encoded data.
func processImageURL(ctx context.Context, imageURL string) (mimeType string, base64Data string, err error) {
	if strings.HasPrefix(imageURL, "data:") {
		parts := strings.SplitN(imageURL, ",", 2)
		if len(parts) != 2 {
			return "", "", fmt.Errorf("invalid data URI")
		}
		prefix := parts[0]
		base64Data = parts[1]

		mimeType = strings.TrimPrefix(prefix, "data:")
		mimeType = strings.TrimSuffix(mimeType, ";base64")
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		return mimeType, base64Data, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("creating request: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("fetching image: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("unexpected status code fetching image: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 20*1024*1024))
	if err != nil {
		return "", "", fmt.Errorf("reading image body: %w", err)
	}

	mimeType = resp.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = http.DetectContentType(body)
	}

	base64Data = base64.StdEncoding.EncodeToString(body)
	return mimeType, base64Data, nil
}

// AgentProcess interface defines the methods required by the server to interact with an agent process.
type AgentProcess interface {
	Client() *acp.AgentClient
	AcquireSession(ctx context.Context, modelName string) (string, error)
	AcquireOrReuseSession(ctx context.Context, lookupHash string, modelName string) (string, bool, error)
	ReleaseSession(sessionID string, stateHash string)
	SubscribeUpdates(sessionID string) (<-chan *acp.SessionUpdate, func())
	SupportsSetConfigOption() bool
	SessionOpened()
	SessionClosed()
}

// AgentGetter is a function that returns an AgentProcess
type AgentGetter func(name, modelName string) (AgentProcess, error)

// StreamEvent represents a categorized ACP update for protocol-specific handlers.
type StreamEvent struct {
	Type string // "text", "thinking", "error", "done"
	Text string
}

// resolveAgent parses a model string into agent config and validates it.
func resolveAgent(cfg *config.Config, model string) (agentCfg *config.AgentConfig, agentName, internalModel string, err error) {
	parts := strings.SplitN(model, "/", 2)
	if len(parts) == 1 {
		agentName = parts[0]
	} else {
		agentName = parts[0]
		internalModel = parts[1]
	}
	ac, ok := cfg.AgentByName(agentName)
	if !ok {
		return nil, "", "", fmt.Errorf("agent %s not found in configuration", agentName)
	}

	if len(parts) == 1 && len(ac.Models) > 0 {
		return nil, "", "", fmt.Errorf("agent %s requires a sub-model (format: %s/<model>)", agentName, agentName)
	}

	if internalModel != "" {
		found := false
		if len(ac.Models) > 0 {
			for _, m := range ac.Models {
				if m.Name == internalModel {
					found = true
					break
				}
			}
		}
		if !found {
			return nil, "", "", fmt.Errorf("model %s is not supported", internalModel)
		}
	}

	return &ac, agentName, internalModel, nil
}

type NormalizedMessage struct {
	Role   string
	Blocks []acp.PromptBlock
}

type RequestContext struct {
	APIKey   string
	Model    string
	System   NormalizedMessage
	Messages []NormalizedMessage
}

func HashMessages(apiKey, model string, system NormalizedMessage, msgs []NormalizedMessage) string {
	h := sha256.New()
	h.Write([]byte(apiKey + "\n"))
	h.Write([]byte(model + "\n"))
	h.Write([]byte("system\n"))
	for _, b := range system.Blocks {
		h.Write([]byte(b.Type + "\n"))
		h.Write([]byte(b.Text + "\n"))
		h.Write([]byte(b.MimeType + "\n"))
		h.Write([]byte(b.Data + "\n"))
	}
	for _, msg := range msgs {
		h.Write([]byte(msg.Role + "\n"))
		for _, b := range msg.Blocks {
			// Ignore thought/thinking blocks for stable matching
			if b.Type == "thought" || b.Type == "thinking" {
				continue
			}
			h.Write([]byte(b.Type + "\n"))
			h.Write([]byte(b.Text + "\n"))
			h.Write([]byte(b.MimeType + "\n"))
			h.Write([]byte(b.Data + "\n"))
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// runACPSession manages the full ACP session lifecycle, routing, and diff prompting.
func runACPSession(
	ctx context.Context,
	proc AgentProcess,
	agentName, internalModel string,
	agentCfg *config.AgentConfig,
	reqCtx RequestContext,
	onEvent func(StreamEvent),
) {
	proc.SessionOpened()
	defer proc.SessionClosed()

	n := len(reqCtx.Messages)
	var prevHash string
	if n > 0 {
		prevHash = HashMessages(reqCtx.APIKey, reqCtx.Model, reqCtx.System, reqCtx.Messages[:n-1])
	} else {
		prevHash = HashMessages(reqCtx.APIKey, reqCtx.Model, reqCtx.System, nil)
	}

	// 2. Acquire or Reuse Session
	sessionID, isReuse, err := proc.AcquireOrReuseSession(ctx, prevHash, internalModel)
	if err != nil {
		log.Printf("Failed to acquire session for agent %s: %v", agentName, err)
		onEvent(StreamEvent{Type: "error", Text: fmt.Sprintf("Failed to acquire session: %v", err)})
		return
	}
	if isReuse {
		log.Printf("Reused session %s for agent %s %s (lookup hash matched)", sessionID, agentName, internalModel)
	} else {
		log.Printf("Acquired new session %s for agent %s %s", sessionID, agentName, internalModel)
	}

	promptFailed := false
	defer func() {
		if promptFailed || ctx.Err() != nil {
			closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := proc.Client().CloseSession(closeCtx, sessionID); err != nil {
				log.Printf("Failed to close session %s for agent %s %s: %v", sessionID, agentName, internalModel, err)
			} else {
				log.Printf("Closed session %s for agent %s %s", sessionID, agentName, internalModel)
			}
		}
	}()



	// 3. Build Prompt
	var prompt []acp.PromptBlock
	if !isReuse {
		// Send system + all messages
		if len(reqCtx.System.Blocks) > 0 {
			prompt = append(prompt, acp.PromptBlock{Type: "text", Text: "[system]:\n"})
			prompt = append(prompt, reqCtx.System.Blocks...)
			prompt = append(prompt, acp.PromptBlock{Type: "text", Text: "\n\n"})
		}
		for _, msg := range reqCtx.Messages {
			if len(msg.Blocks) == 0 {
				continue
			}
			prompt = append(prompt, acp.PromptBlock{Type: "text", Text: fmt.Sprintf("[%s]:\n", msg.Role)})
			prompt = append(prompt, msg.Blocks...)
			prompt = append(prompt, acp.PromptBlock{Type: "text", Text: "\n\n"})
		}
	} else {
		// Send ONLY the last message
		if n > 0 {
			msg := reqCtx.Messages[n-1]
			prompt = append(prompt, acp.PromptBlock{Type: "text", Text: fmt.Sprintf("[%s]:\n", msg.Role)})
			prompt = append(prompt, msg.Blocks...)
			prompt = append(prompt, acp.PromptBlock{Type: "text", Text: "\n\n"})
		}
	}

	updates, unsubscribe := proc.SubscribeUpdates(sessionID)
	defer func() {
		if unsubscribe != nil {
			unsubscribe()
		}
	}()

	promptDone := make(chan error, 1)
	go func() {
		if len(prompt) > 0 {
			promptDone <- proc.Client().Prompt(ctx, sessionID, prompt)
		} else {
			promptDone <- nil
		}
	}()

	promptFinished := false
	timer := time.NewTimer(5 * time.Minute)
	defer timer.Stop()

	var accumulatedText string

	for !promptFinished {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.Canceled {
				log.Printf("Client disconnected, canceling session %s", sessionID)
				proc.Client().CancelSession(context.Background(), sessionID)
			}
			return
		case err := <-promptDone:
			if err != nil {
				log.Printf("Prompt failed for agent %s session %s: %v", agentName, sessionID, err)
				promptFailed = true
			}
			promptFinished = true
		case update := <-updates:
			timer.Reset(5 * time.Minute)
			if update == nil {
				continue
			}

			updateType := update.Update.SessionUpdate
			if updateType == "" {
				updateType = update.Event.Type
			}

			switch updateType {
			case "agent_message_chunk":
				if update.Update.Content.Type == "text" && update.Update.Content.Text != "" {
					accumulatedText += update.Update.Content.Text
					onEvent(StreamEvent{Type: "text", Text: update.Update.Content.Text})
				} else if update.Update.Content.Type == "thought" && update.Update.Content.Text != "" {
					onEvent(StreamEvent{Type: "thinking", Text: update.Update.Content.Text})
				}
			case "agent_thought_chunk":
				if update.Update.Content.Type == "text" && update.Update.Content.Text != "" {
					onEvent(StreamEvent{Type: "thinking", Text: update.Update.Content.Text})
				}
			case "message_delta":
				var delta acp.MessageDelta
				if err := json.Unmarshal(update.Event.Delta, &delta); err == nil && delta.Text != "" {
					accumulatedText += delta.Text
					onEvent(StreamEvent{Type: "text", Text: delta.Text})
				}
			case "error":
				log.Printf("Agent %s sent error event for session %s", agentName, sessionID)
				promptFailed = true
				promptFinished = true
			}
		case <-timer.C:
			log.Printf("Timed out waiting for agent %s session %s", agentName, sessionID)
			promptFailed = true
			promptFinished = true
		}
	}

	onEvent(StreamEvent{Type: "done"})

	// 4. Update State Hash and Release Session
	if !promptFailed && ctx.Err() == nil {
		assistantMsg := NormalizedMessage{
			Role: "assistant",
			Blocks: []acp.PromptBlock{
				{Type: "text", Text: accumulatedText},
			},
		}
		// Final state hash includes System + all original messages + accumulated assistant response
		finalHash := HashMessages(reqCtx.APIKey, reqCtx.Model, reqCtx.System, append(reqCtx.Messages, assistantMsg))
		proc.ReleaseSession(sessionID, finalHash)
	}
}
