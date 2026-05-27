package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"

	"acp-gateway/acp"
	"acp-gateway/config"
)

// --- Claude Request Types ---

type ClaudeImageSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // e.g. "image/png"
	Data      string `json:"data"`       // base64-encoded data
}

type ClaudeContentBlock struct {
	Type   string             `json:"type"`             // "text", "image"
	Text   string             `json:"text,omitempty"`   // for type "text"
	Source *ClaudeImageSource `json:"source,omitempty"` // for type "image"
}

type ClaudeMessageContent struct {
	IsString bool
	Text     string
	Blocks   []ClaudeContentBlock
}

func (c *ClaudeMessageContent) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		c.IsString = true
		return json.Unmarshal(data, &c.Text)
	}
	c.IsString = false
	return json.Unmarshal(data, &c.Blocks)
}

type ClaudeMessage struct {
	Role    string               `json:"role"`
	Content ClaudeMessageContent `json:"content"`
}

type ClaudeSystemContent struct {
	IsString bool
	Text     string
	Blocks   []ClaudeContentBlock
}

func (c *ClaudeSystemContent) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		c.IsString = true
		return json.Unmarshal(data, &c.Text)
	}
	c.IsString = false
	return json.Unmarshal(data, &c.Blocks)
}

type ClaudeRequest struct {
	Model     string               `json:"model"`
	Messages  []ClaudeMessage      `json:"messages"`
	System    *ClaudeSystemContent `json:"system,omitempty"`
	MaxTokens int                  `json:"max_tokens"`
	Stream    bool                 `json:"stream"`
}

// --- Claude SSE Response Types ---

type claudeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type claudeMessageStart struct {
	Type    string `json:"type"` // "message_start"
	Message struct {
		ID           string        `json:"id"`
		Type         string        `json:"type"` // "message"
		Role         string        `json:"role"` // "assistant"
		Content      []interface{} `json:"content"`
		Model        string        `json:"model"`
		StopReason   *string       `json:"stop_reason"`
		StopSequence *string       `json:"stop_sequence"`
		Usage        claudeUsage   `json:"usage"`
	} `json:"message"`
}

type claudeContentBlockStart struct {
	Type         string      `json:"type"` // "content_block_start"
	Index        int         `json:"index"`
	ContentBlock interface{} `json:"content_block"`
}

type claudeContentBlockDelta struct {
	Type  string      `json:"type"` // "content_block_delta"
	Index int         `json:"index"`
	Delta interface{} `json:"delta"`
}

type claudeContentBlockStop struct {
	Type  string `json:"type"` // "content_block_stop"
	Index int    `json:"index"`
}

type claudeMessageDelta struct {
	Type  string `json:"type"` // "message_delta"
	Delta struct {
		StopReason   string  `json:"stop_reason"`
		StopSequence *string `json:"stop_sequence"`
	} `json:"delta"`
	Usage claudeUsage `json:"usage"`
}

type claudeMessageStop struct {
	Type string `json:"type"` // "message_stop"
}

type ClaudeContentResponse struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
}

type ClaudeMessageResponse struct {
	ID           string                  `json:"id"`
	Type         string                  `json:"type"`
	Role         string                  `json:"role"`
	Model        string                  `json:"model"`
	Content      []ClaudeContentResponse `json:"content"`
	StopReason   string                  `json:"stop_reason"`
	StopSequence *string                 `json:"stop_sequence"`
	Usage        claudeUsage             `json:"usage"`
}



// --- Claude SSE helpers ---

func sendClaudeSSE(w io.Writer, flusher http.Flusher, eventType string, data interface{}) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, string(b))
	flusher.Flush()
}

// --- Handler ---

func HandleClaude(cfg *config.Config, getAgent AgentGetter) http.HandlerFunc {
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

		var req ClaudeRequest
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
			log.Printf("Claude request failed while getting agent %s: %v", agentName, err)
			http.Error(w, "Agent not available", http.StatusInternalServerError)
			return
		}

		ctx := r.Context()
		apiKey, _ := ctx.Value(APIKeyContextKey).(string)

		var reqCtx RequestContext
		reqCtx.APIKey = apiKey
		reqCtx.Model = req.Model

		var sysBlocks []acp.PromptBlock
		if req.System != nil {
			if req.System.IsString {
				sysBlocks = append(sysBlocks, acp.PromptBlock{Type: "text", Text: req.System.Text + "\n\n"})
			} else {
				for _, block := range req.System.Blocks {
					if block.Type == "text" {
						sysBlocks = append(sysBlocks, acp.PromptBlock{Type: "text", Text: block.Text + "\n\n"})
					}
				}
			}
		}
		reqCtx.System = NormalizedMessage{Role: "system", Blocks: sysBlocks}

		for _, msg := range req.Messages {
			var blocks []acp.PromptBlock
			if msg.Content.IsString {
				blocks = append(blocks, acp.PromptBlock{Type: "text", Text: msg.Content.Text})
			} else {
				for _, block := range msg.Content.Blocks {
					if block.Type == "text" {
						blocks = append(blocks, acp.PromptBlock{Type: "text", Text: block.Text})
					} else if block.Type == "image" && block.Source != nil {
						blocks = append(blocks, acp.PromptBlock{
							Type:     "image",
							MimeType: block.Source.MediaType,
							Data:     block.Source.Data,
						})
					}
				}
			}
			reqCtx.Messages = append(reqCtx.Messages, NormalizedMessage{Role: msg.Role, Blocks: blocks})
		}



		messageID := fmt.Sprintf("msg_%s", newUUID())

		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
				return
			}

			// Send message_start
			msgStart := claudeMessageStart{Type: "message_start"}
			msgStart.Message.ID = messageID
			msgStart.Message.Type = "message"
			msgStart.Message.Role = "assistant"
			msgStart.Message.Content = []interface{}{}
			msgStart.Message.Model = req.Model
			msgStart.Message.Usage = claudeUsage{InputTokens: 0, OutputTokens: 0}
			sendClaudeSSE(w, flusher, "message_start", msgStart)

			// Track content block state
			thinkingBlockOpen := false
			textBlockOpen := false
			thinkingIndex := 0
			textIndex := 0
			outputTokens := 0

			runACPSession(ctx, proc, agentName, internalModel, agentCfg, reqCtx, func(ev StreamEvent) {
				switch ev.Type {
				case "thinking":
					if !thinkingBlockOpen {
						// Open thinking content block
						thinkingIndex = 0
						if textBlockOpen {
							thinkingIndex = 1 // shouldn't happen, but be safe
						}
						sendClaudeSSE(w, flusher, "content_block_start", claudeContentBlockStart{
							Type:  "content_block_start",
							Index: thinkingIndex,
							ContentBlock: map[string]string{
								"type":      "thinking",
								"thinking":  "",
								"signature": "",
							},
						})
						thinkingBlockOpen = true
						textIndex = thinkingIndex + 1
					}
					sendClaudeSSE(w, flusher, "content_block_delta", claudeContentBlockDelta{
						Type:  "content_block_delta",
						Index: thinkingIndex,
						Delta: map[string]string{
							"type":     "thinking_delta",
							"thinking": ev.Text,
						},
					})
					outputTokens++

				case "text":
					if thinkingBlockOpen && !textBlockOpen {
						// Close thinking block before opening text block
						sendClaudeSSE(w, flusher, "content_block_stop", claudeContentBlockStop{
							Type:  "content_block_stop",
							Index: thinkingIndex,
						})
						thinkingBlockOpen = false
					}
					if !textBlockOpen {
						sendClaudeSSE(w, flusher, "content_block_start", claudeContentBlockStart{
							Type:  "content_block_start",
							Index: textIndex,
							ContentBlock: map[string]string{
								"type": "text",
								"text": "",
							},
						})
						textBlockOpen = true
					}
					sendClaudeSSE(w, flusher, "content_block_delta", claudeContentBlockDelta{
						Type:  "content_block_delta",
						Index: textIndex,
						Delta: map[string]string{
							"type": "text_delta",
							"text": ev.Text,
						},
					})
					outputTokens++

				case "done":
					// Close any open thinking block
					if thinkingBlockOpen {
						sendClaudeSSE(w, flusher, "content_block_stop", claudeContentBlockStop{
							Type:  "content_block_stop",
							Index: thinkingIndex,
						})
					}

					// If no text block was ever opened, open an empty one
					if !textBlockOpen {
						sendClaudeSSE(w, flusher, "content_block_start", claudeContentBlockStart{
							Type:  "content_block_start",
							Index: textIndex,
							ContentBlock: map[string]string{
								"type": "text",
								"text": "",
							},
						})
					}

					// Close text block
					sendClaudeSSE(w, flusher, "content_block_stop", claudeContentBlockStop{
						Type:  "content_block_stop",
						Index: textIndex,
					})

					// Send message_delta with stop_reason
					msgDelta := claudeMessageDelta{Type: "message_delta"}
					msgDelta.Delta.StopReason = "end_turn"
					msgDelta.Usage = claudeUsage{OutputTokens: outputTokens}
					sendClaudeSSE(w, flusher, "message_delta", msgDelta)

					// Send message_stop
					sendClaudeSSE(w, flusher, "message_stop", claudeMessageStop{Type: "message_stop"})

				case "error":
					// If we haven't opened any blocks yet, open a text block with the error
					if !textBlockOpen {
						sendClaudeSSE(w, flusher, "content_block_start", claudeContentBlockStart{
							Type:  "content_block_start",
							Index: textIndex,
							ContentBlock: map[string]string{
								"type": "text",
								"text": "",
							},
						})
					}
					sendClaudeSSE(w, flusher, "content_block_stop", claudeContentBlockStop{
						Type:  "content_block_stop",
						Index: textIndex,
					})
					msgDelta := claudeMessageDelta{Type: "message_delta"}
					msgDelta.Delta.StopReason = "end_turn"
					sendClaudeSSE(w, flusher, "message_delta", msgDelta)
					sendClaudeSSE(w, flusher, "message_stop", claudeMessageStop{Type: "message_stop"})
				}
			})
		} else {
			var fullText string
			var fullThinking string
			var promptErr error
			outputTokens := 0

			runACPSession(ctx, proc, agentName, internalModel, agentCfg, reqCtx, func(ev StreamEvent) {
				switch ev.Type {
				case "text":
					fullText += ev.Text
					outputTokens++
				case "thinking":
					fullThinking += ev.Text
					outputTokens++
				case "error":
					promptErr = fmt.Errorf("%s", ev.Text)
				}
			})

			if promptErr != nil && fullText == "" {
				http.Error(w, promptErr.Error(), http.StatusInternalServerError)
				return
			}

			content := make([]ClaudeContentResponse, 0)
			if fullThinking != "" {
				content = append(content, ClaudeContentResponse{
					Type:     "thinking",
					Thinking: fullThinking,
				})
			}
			if fullText != "" || fullThinking == "" {
				content = append(content, ClaudeContentResponse{
					Type: "text",
					Text: fullText,
				})
			}

			resp := ClaudeMessageResponse{
				ID:         messageID,
				Type:       "message",
				Role:       "assistant",
				Model:      req.Model,
				Content:    content,
				StopReason: "end_turn",
				Usage: claudeUsage{
					InputTokens:  0,
					OutputTokens: outputTokens,
				},
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		}
	}
}
