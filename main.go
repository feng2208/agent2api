package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"acp-gateway/config"
	"acp-gateway/server"
)

//go:embed config-template.yaml
var configTemplate string

var Version = "dev"

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func main() {
	configPath := flag.String("config", "config.yaml", "path to configuration file")
	printTmpl := flag.Bool("template", false, "print configuration template")
	debug := flag.Bool("debug", false, "enable debug logging for agent communication")
	versionFlag := flag.Bool("version", false, "print version information")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("agent2api version: %s\n", Version)
		os.Exit(0)
	}

	if *printTmpl {
		fmt.Print(configTemplate)
		os.Exit(0)
	}

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Check if it's the config file itself that's missing
			if _, statErr := os.Stat(*configPath); errors.Is(statErr, os.ErrNotExist) {
				log.Fatalf("Config file '%s' not found.\nUse '-template' to print the default configuration template.", *configPath)
			}
		}
		log.Fatalf("Failed to load config: %v", err)
	}

	pm := NewProcessManager(cfg, *debug)

	// Eagerly start all configured agents with MaxIdleSessions > 0
	for _, a := range cfg.Agents {
		if !a.HasExtraArgs() {
			if a.MaxIdleSessions() > 0 && len(a.Models) > 0 {
				if _, err := pm.GetAgent(a.Name, a.Models[0].Name); err != nil {
					log.Printf("Failed to eagerly start shared agent %s: %v", a.Name, err)
				}
			}
		} else {
			for _, m := range a.Models {
				if m.MaxIdleSessions > 0 {
					if _, err := pm.GetAgent(a.Name, m.Name); err != nil {
						log.Printf("Failed to eagerly start agent %s with model %s: %v", a.Name, m.Name, err)
					}
				}
			}
		}
	}

	validKeys := make(map[string]struct{}, len(cfg.APIKeys))
	for _, k := range cfg.APIKeys {
		validKeys[k] = struct{}{}
	}

	// Middleware for API Keys (supports both OpenAI "Authorization: Bearer" and Claude "x-api-key")
	authMiddleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var token string
			if apiKey := r.Header.Get("x-api-key"); apiKey != "" {
				token = apiKey
			} else {
				authHeader := r.Header.Get("Authorization")
				token = strings.TrimPrefix(authHeader, "Bearer ")
			}

			if _, valid := validKeys[token]; !valid {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			ctx := context.WithValue(r.Context(), server.APIKeyContextKey, token)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}

	// Middleware for logging client requests to the terminal.
	logMiddleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			recorder := &statusRecorder{ResponseWriter: w}

			next.ServeHTTP(recorder, r)

			status := recorder.status
			if status == 0 {
				status = http.StatusOK
			}

			log.Printf(
				"%s %s %s from %s -> %d %dB in %s",
				r.Method,
				r.URL.RequestURI(),
				r.Proto,
				r.RemoteAddr,
				status,
				recorder.bytes,
				time.Since(start).Round(time.Millisecond),
			)
		})
	}

	agentGetter := func(name, modelName string) (server.AgentProcess, error) {
		return pm.GetAgent(name, modelName)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", server.HandleModels(cfg))
	mux.HandleFunc("/v1/chat/completions", server.HandleChat(cfg, agentGetter))
	mux.HandleFunc("/v1/messages", server.HandleClaude(cfg, agentGetter))

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: logMiddleware(authMiddleware(mux)),
	}

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-quit
		log.Printf("Received signal %v, shutting down gracefully...", sig)

		// Clean up processes
		pm.mu.Lock()
		for _, p := range pm.procs {
			p.Close()
		}
		pm.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Fatalf("Server forced to shutdown: %v", err)
		}
	}()

	log.Printf("Gateway listening on %s", cfg.Listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}
