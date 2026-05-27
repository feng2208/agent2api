package server

import (
	"encoding/json"
	"net/http"

	"acp-gateway/config"
)

type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type ModelsResponse struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

func HandleModels(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var models []Model
		for _, agent := range cfg.Agents {
			if len(agent.Models) == 0 {
				// Add the agent itself as a model only if no models are defined
				models = append(models, Model{
					ID:      agent.Name,
					Object:  "model",
					Created: 1677610602,
					OwnedBy: agent.Name,
				})
			}
			for _, m := range agent.Models {
				// Combine agent name and model
				id := agent.Name + "/" + m.Name
				models = append(models, Model{
					ID:      id,
					Object:  "model",
					Created: 1677610602,
					OwnedBy: agent.Name,
				})
			}
		}

		resp := ModelsResponse{
			Object: "list",
			Data:   models,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}
