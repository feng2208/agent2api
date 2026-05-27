package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"acp-gateway/acp"
	"acp-gateway/config"
)

type ChatMessage struct {
	Role    string         `json:"role"`
	Content MessageContent `json:"content"`
}

type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type ChatDelta struct {
	Role             string `json:"role,omitempty"`
	Content          string `json:"content,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type ChatChoice struct {
	Delta        ChatDelta `json:"delta"`
	Index        int       `json:"index"`
	FinishReason *string   `json:"finish_reason,omitempty"`
}

type ChatChunk struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
}

type ChatMessageResponse struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type ChatChoiceNonStream struct {
	Index        int                 `json:"index"`
	Message      ChatMessageResponse `json:"message"`
	FinishReason string              `json:"finish_reason"`
}

type ChatCompletion struct {
	ID      string                `json:"id"`
	Object  string                `json:"object"`
	Created int64                 `json:"created"`
	Model   string                `json:"model"`
	Choices []ChatChoiceNonStream `json:"choices"`
	Usage   map[string]int        `json:"usage"`
}

func HandleChat(cfg *config.Config, getAgent AgentGetter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}

		var req ChatRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		agentCfg, agentName, internalModel, err := resolveAgent(cfg, req.Model)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		proc, err := getAgent(agentName, internalModel)
		if err != nil {
			http.Error(w, "Agent not available", http.StatusInternalServerError)
			return
		}

		ctx := r.Context()
		apiKey, _ := ctx.Value(APIKeyContextKey).(string)

		var reqCtx RequestContext
		reqCtx.APIKey = apiKey
		reqCtx.Model = req.Model
		var sysBlocks []acp.PromptBlock

		for _, msg := range req.Messages {
			if msg.Role == "system" {
				if msg.Content.IsString {
					sysBlocks = append(sysBlocks, acp.PromptBlock{Type: "text", Text: msg.Content.Text + "\n\n"})
				} else {
					for _, part := range msg.Content.Parts {
						if part.Type == "text" {
							sysBlocks = append(sysBlocks, acp.PromptBlock{Type: "text", Text: part.Text + "\n\n"})
						}
					}
				}
			} else {
				var blocks []acp.PromptBlock
				if msg.Content.IsString {
					blocks = append(blocks, acp.PromptBlock{Type: "text", Text: msg.Content.Text})
				} else {
					for _, part := range msg.Content.Parts {
						if part.Type == "text" {
							blocks = append(blocks, acp.PromptBlock{Type: "text", Text: part.Text})
						} else if part.Type == "image_url" && part.ImageURL != nil {
							mimeType, b64, err := processImageURL(ctx, part.ImageURL.URL)
							if err != nil {
								http.Error(w, fmt.Sprintf("failed to process image: %v", err), http.StatusBadRequest)
								return
							}
							blocks = append(blocks, acp.PromptBlock{Type: "image", MimeType: mimeType, Data: b64})
						}
					}
				}
				reqCtx.Messages = append(reqCtx.Messages, NormalizedMessage{Role: msg.Role, Blocks: blocks})
			}
		}
		reqCtx.System = NormalizedMessage{Role: "system", Blocks: sysBlocks}

		created := time.Now().Unix()
		completionID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())

		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
				return
			}

			// Send initial role
			sendChunk(w, flusher, completionID, req.Model, created, "assistant", "", "")

			runACPSession(ctx, proc, agentName, internalModel, agentCfg, reqCtx, func(ev StreamEvent) {
				switch ev.Type {
				case "text":
					sendChunk(w, flusher, completionID, req.Model, created, "", ev.Text, "")
				case "thinking":
					sendChunk(w, flusher, completionID, req.Model, created, "", "", ev.Text)
				case "done":
					sendChunkDone(w, flusher, completionID, req.Model, created)
				}
			})
		} else {
			var fullText string
			var fullThinking string
			var promptErr error

			runACPSession(ctx, proc, agentName, internalModel, agentCfg, reqCtx, func(ev StreamEvent) {
				switch ev.Type {
				case "text":
					fullText += ev.Text
				case "thinking":
					fullThinking += ev.Text
				case "error":
					promptErr = fmt.Errorf("%s", ev.Text)
				}
			})

			if promptErr != nil && fullText == "" {
				http.Error(w, promptErr.Error(), http.StatusInternalServerError)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			resp := ChatCompletion{
				ID:      completionID,
				Object:  "chat.completion",
				Created: created,
				Model:   req.Model,
				Choices: []ChatChoiceNonStream{
					{
						Index: 0,
						Message: ChatMessageResponse{
							Role:             "assistant",
							Content:          fullText,
							ReasoningContent: fullThinking,
						},
						FinishReason: "stop",
					},
				},
				Usage: map[string]int{
					"prompt_tokens":     0,
					"completion_tokens": 0,
					"total_tokens":      0,
				},
			}
			json.NewEncoder(w).Encode(resp)
		}
	}
}

func sendChunk(w io.Writer, flusher http.Flusher, id, model string, created int64, role, content, reasoningContent string) {
	chunk := ChatChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
	}
	chunk.Choices = []ChatChoice{
		{
			Index:        0,
			FinishReason: nil,
		},
	}

	if role != "" {
		chunk.Choices[0].Delta.Role = role
	}
	if content != "" {
		chunk.Choices[0].Delta.Content = content
	}
	if reasoningContent != "" {
		chunk.Choices[0].Delta.ReasoningContent = reasoningContent
	}

	b, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", string(b))
	flusher.Flush()
}

func sendChunkDone(w io.Writer, flusher http.Flusher, id, model string, created int64) {
	chunk := ChatChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
	}

	stop := "stop"
	chunk.Choices = []ChatChoice{
		{
			Index:        0,
			FinishReason: &stop,
		},
	}

	b, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", string(b))
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}
