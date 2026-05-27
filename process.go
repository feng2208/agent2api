package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"

	"acp-gateway/acp"
	"acp-gateway/config"
)

type SessionState struct {
	SessionID string
	StateHash string
	LastUsed  time.Time
}

type AgentProcess struct {
	Config   config.AgentConfig
	servedModels []config.ModelConfig
	name     string
	model    string // the internal model name assigned to this process
	cmd     *exec.Cmd
	client  *acp.AgentClient
	updates chan *acp.SessionUpdate
	caps    acp.AgentCapabilities

	idleSessions map[string]chan string
	ctx          context.Context
	cancel       context.CancelFunc

	mu             sync.Mutex
	activeSessions int
	lastActive     time.Time
	updateSubs     map[string]chan *acp.SessionUpdate

	routedSessions map[string]*SessionState
	sessionMu      sync.Mutex

	ready   chan struct{}
	initErr error
	debug   bool
}

type ProcessManager struct {
	configs map[string]config.AgentConfig
	procs   map[string]*AgentProcess
	mu      sync.Mutex
	debug   bool
}

func NewProcessManager(cfg *config.Config, debug bool) *ProcessManager {
	configs := make(map[string]config.AgentConfig)
	for _, a := range cfg.Agents {
		configs[a.Name] = a
	}
	return &ProcessManager{
		configs: configs,
		procs:   make(map[string]*AgentProcess),
		debug:   debug,
	}
}

func (pm *ProcessManager) GetAgent(name, modelName string) (*AgentProcess, error) {
	pm.mu.Lock()

	cfg, exists := pm.configs[name]
	if !exists {
		pm.mu.Unlock()
		return nil, fmt.Errorf("agent %s not found in configuration", name)
	}

	var matchedModel config.ModelConfig
	found := false
	if modelName != "" {
		for _, m := range cfg.Models {
			if m.Name == modelName {
				matchedModel = m
				found = true
				break
			}
		}
	}
	if !found && len(cfg.Models) > 0 {
		pm.mu.Unlock()
		return nil, fmt.Errorf("model %q not supported by agent %s", modelName, name)
	}

	isShared := !cfg.HasExtraArgs()

	key := name
	if !isShared && modelName != "" {
		key = name + ":" + modelName
	}

	p, exists := pm.procs[key]
	if exists {
		pm.mu.Unlock()
		<-p.ready // Wait for initialization to complete
		if p.initErr != nil {
			return nil, p.initErr
		}
		if p.cmd != nil && p.cmd.ProcessState == nil {
			p.touch()
			return p, nil
		}
		
		// Process died, we need to restart it
		pm.mu.Lock()
		// Double check if another goroutine already restarted it
		p2, exists2 := pm.procs[key]
		if exists2 && p2 != p {
			pm.mu.Unlock()
			return pm.GetAgent(name, modelName)
		}
	}

	log.Printf("Starting agent process: %s", key)

	ctx, cancel := context.WithCancel(context.Background())

	var servedModels []config.ModelConfig
	idleSessions := make(map[string]chan string)
	var procModel string

	if isShared {
		servedModels = cfg.Models
		for _, m := range cfg.Models {
			idleCount := m.MaxIdleSessions
			if idleCount < 0 {
				idleCount = 0
			}
			idleSessions[m.Name] = make(chan string, idleCount)
		}
		procModel = ""
	} else {
		servedModels = []config.ModelConfig{matchedModel}
		idleCount := matchedModel.MaxIdleSessions
		if idleCount < 0 {
			idleCount = 0
		}
		idleSessions[matchedModel.Name] = make(chan string, idleCount)
		procModel = modelName
	}

	p = &AgentProcess{
		Config:         cfg,
		servedModels:   servedModels,
		name:           name,
		model:          procModel,
		updates:        make(chan *acp.SessionUpdate, 100),
		idleSessions:   idleSessions,
		ctx:            ctx,
		cancel:         cancel,
		lastActive:     time.Now(),
		updateSubs:     make(map[string]chan *acp.SessionUpdate),
		routedSessions: make(map[string]*SessionState),
		ready:          make(chan struct{}),
		debug:          pm.debug,
	}

	pm.procs[key] = p
	pm.mu.Unlock() // Unlock before starting to allow parallel initialization

	err := p.start(pm, key)
	p.initErr = err
	close(p.ready)

	if err != nil {
		log.Printf("Failed to start agent process %s: %v", key, err)
		pm.mu.Lock()
		if pm.procs[key] == p {
			delete(pm.procs, key)
		}
		pm.mu.Unlock()
		return nil, err
	}

	return p, nil
}

func (p *AgentProcess) start(pm *ProcessManager, key string) error {
	commandPath, err := exec.LookPath(p.Config.Command)
	if err != nil {
		return fmt.Errorf("agent command %q is not available: %w", p.Config.Command, err)
	}

	args := append([]string(nil), p.Config.Args...)
	if len(p.servedModels) == 1 {
		m := p.servedModels[0]
		if len(m.ExtraArgs) > 0 {
			args = append(args, m.ExtraArgs...)
		}
	}

	p.cmd = exec.Command(commandPath, args...)
	p.cmd.Stderr = os.Stderr
	p.cmd.Dir = p.Config.Cwd

	stdout, err := p.cmd.StdoutPipe()
	if err != nil {
		return err
	}

	stdin, err := p.cmd.StdinPipe()
	if err != nil {
		return err
	}

	if err := p.cmd.Start(); err != nil {
		return fmt.Errorf("starting command %q: %w", p.Config.Command, err)
	}

	rpc := acp.NewRPCClient(stdout, stdin)
	rpc.Debug = pm.debug
	rpc.RequestHandler = acp.DefaultRequestHandler
	rpc.NotificationHandler = func(req *acp.JSONRPCRequest) {
		if req.Method == "session/update" {
			var update acp.SessionUpdate
			if err := json.Unmarshal(req.Params, &update); err == nil {
				p.publishUpdate(&update)
			}
		}
	}

	p.client = acp.NewAgentClient(rpc)
	go rpc.Start()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	initResp, err := p.client.Initialize(ctx)
	if err != nil {
		log.Printf("Failed to initialize ACP agent %s: %v", key, err)
		p.cmd.Process.Kill()
		return fmt.Errorf("initializing ACP agent %s: %w", key, err)
	}
	if err := p.authenticate(ctx, key, initResp.AuthMethods); err != nil {
		p.cmd.Process.Kill()
		return err
	}
	p.caps = initResp.AgentCapabilities

	log.Printf("Agent process %s started successfully (pid %d)", key, p.cmd.Process.Pid)

	// Start idle session maintainer
	hasIdle := false
	for _, m := range p.servedModels {
		if m.MaxIdleSessions > 0 {
			hasIdle = true
			break
		}
	}
	if hasIdle {
		go p.maintainIdleSessions()
	}

	go func() {
		err := p.cmd.Wait()
		if err != nil {
			log.Printf("Agent process %s exited with error: %v", key, err)
		} else {
			log.Printf("Agent process %s exited", key)
		}
		p.cancel() // Stop maintainIdleSessions and other contextual operations

		pm.mu.Lock()
		if pm.procs[key] == p {
			delete(pm.procs, key)
		}
		pm.mu.Unlock()
	}()

	return nil
}

func (p *AgentProcess) maintainIdleSessions() {
	for {
		select {
		case <-p.ctx.Done():
			return
		default:
		}

		// Clean up routed sessions > 20 minutes
		p.sessionMu.Lock()
		now := time.Now()
		for sessID, state := range p.routedSessions {
			if now.Sub(state.LastUsed) > 20*time.Minute {
				delete(p.routedSessions, sessID)
				go p.Client().CloseSession(context.Background(), sessID)
				log.Printf("Evicted inactive routed session %s for agent %s", sessID, p.name)
			}
		}
		p.sessionMu.Unlock()

		createdAny := false
		for _, m := range p.servedModels {
			if m.MaxIdleSessions <= 0 {
				continue
			}

			ch, ok := p.idleSessions[m.Name]
			if !ok {
				continue
			}

			if len(ch) >= m.MaxIdleSessions {
				continue
			}

			ctx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
			resp, err := p.Client().NewSession(ctx, p.Config.Cwd)
			var sessID string
			if err == nil {
				sessID = resp.SessionID
				p.logConfigOptions(sessID, resp.ConfigOptions)

				for _, optMap := range m.Options {
					for k, v := range optMap {
						if errSet := p.Client().SetConfigOption(ctx, sessID, k, v); errSet != nil {
							log.Printf("Agent %s failed to set option %s=%s: %v", p.name, k, v, errSet)
						}
					}
				}

				select {
				case ch <- sessID:
					log.Printf("Pre-created idle session %s for agent %s model %s", sessID, p.name, m.Name)
					createdAny = true
				case <-p.ctx.Done():
					p.Client().CloseSession(context.Background(), sessID)
					cancel()
					return
				}
			} else {
				log.Printf("Agent %s failed to pre-create idle session for model %s: %v", p.name, m.Name, err)
			}
			cancel()
		}

		if !createdAny {
			time.Sleep(1 * time.Second)
		}
	}
}

func (p *AgentProcess) AcquireSession(ctx context.Context, modelName string) (string, error) {
	// Find ModelConfig
	var matchedModel config.ModelConfig
	found := false
	for _, m := range p.servedModels {
		if m.Name == modelName {
			matchedModel = m
			found = true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("model %s not supported by agent %s in this process", modelName, p.name)
	}

	ch, hasChan := p.idleSessions[modelName]
	if hasChan && matchedModel.MaxIdleSessions > 0 {
		select {
		case sessID := <-ch:
			log.Printf("Acquired idle session %s for agent %s %s", sessID, p.name, modelName)
			return sessID, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	resp, err := p.Client().NewSession(ctx, p.Config.Cwd)
	var sessID string
	if err == nil {
		sessID = resp.SessionID
		p.logConfigOptions(sessID, resp.ConfigOptions)

		for _, optMap := range matchedModel.Options {
			for k, v := range optMap {
				if errSet := p.Client().SetConfigOption(ctx, sessID, k, v); errSet != nil {
					log.Printf("Agent %s failed to set option %s=%s: %v", p.name, k, v, errSet)
				}
			}
		}
	}
	return sessID, err
}

func (p *AgentProcess) AcquireOrReuseSession(ctx context.Context, lookupHash string, modelName string) (string, bool, error) {
	p.sessionMu.Lock()
	var matchedSession string

	for sessID, state := range p.routedSessions {
		if state.StateHash == lookupHash {
			matchedSession = sessID
			break
		}
	}

	if matchedSession != "" {
		delete(p.routedSessions, matchedSession)
		p.sessionMu.Unlock()
		return matchedSession, true, nil
	}
	p.sessionMu.Unlock()

	sessID, err := p.AcquireSession(ctx, modelName)
	return sessID, false, err
}

func (p *AgentProcess) ReleaseSession(sessionID string, stateHash string) {
	p.sessionMu.Lock()
	defer p.sessionMu.Unlock()
	p.routedSessions[sessionID] = &SessionState{
		SessionID: sessionID,
		StateHash: stateHash,
		LastUsed:  time.Now(),
	}
}

func (p *AgentProcess) authenticate(ctx context.Context, name string, methods []acp.AuthMethod) error {
	method, ok := selectAuthMethod(methods)
	if !ok {
		return nil
	}

	log.Printf("Authenticating ACP agent %s using method %s", name, method.ID)
	if err := p.client.Authenticate(ctx, method.ID); err != nil {
		log.Printf("Failed to authenticate ACP agent %s using method %s: %v", name, method.ID, err)
		return fmt.Errorf("authenticating ACP agent %s using method %s: %w", name, method.ID, err)
	}

	log.Printf("ACP agent %s authenticated using method %s", name, method.ID)
	return nil
}

func selectAuthMethod(methods []acp.AuthMethod) (acp.AuthMethod, bool) {
	for _, method := range methods {
		if method.Type != "env_var" {
			continue
		}
		for _, envVar := range method.Vars {
			if os.Getenv(envVar.Name) != "" {
				return method, true
			}
		}
	}

	for _, method := range methods {
		if method.Type == "" || method.Type == "agent" {
			return method, true
		}
	}

	return acp.AuthMethod{}, false
}

func (p *AgentProcess) touch() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastActive = time.Now()
}

func (p *AgentProcess) SessionOpened() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.activeSessions++
	p.lastActive = time.Now()
}

func (p *AgentProcess) SessionClosed() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.activeSessions--
	p.lastActive = time.Now()

	if p.activeSessions <= 0 {
		p.activeSessions = 0
	}
}

func (p *AgentProcess) Updates() <-chan *acp.SessionUpdate {
	return p.updates
}

func (p *AgentProcess) SubscribeUpdates(sessionID string) (<-chan *acp.SessionUpdate, func()) {
	ch := make(chan *acp.SessionUpdate, 100)

	p.mu.Lock()
	if p.updateSubs == nil {
		p.updateSubs = make(map[string]chan *acp.SessionUpdate)
	}
	p.updateSubs[sessionID] = ch
	p.mu.Unlock()

	cancel := func() {
		p.mu.Lock()
		if current, ok := p.updateSubs[sessionID]; ok && current == ch {
			delete(p.updateSubs, sessionID)
			close(ch)
		}
		p.mu.Unlock()
	}

	return ch, cancel
}

func (p *AgentProcess) publishUpdate(update *acp.SessionUpdate) {
	p.mu.Lock()
	ch := p.updateSubs[update.SessionID]
	p.mu.Unlock()

	if ch != nil {
		select {
		case ch <- update:
		default:
			log.Printf("Warning: dropped update for session %s (subscriber channel full)", update.SessionID)
		}
		return
	}

	select {
	case p.updates <- update:
	default:
		log.Printf("Warning: dropped global update for agent %s (updates channel full)", p.name)
	}
}

func (p *AgentProcess) Client() *acp.AgentClient {
	return p.client
}

func (p *AgentProcess) SupportsSetConfigOption() bool {
	return p.caps.SessionCapabilities.SetConfigOption != nil
}

func (p *AgentProcess) Close() {
	p.cancel()
	if p.cmd != nil && p.cmd.Process != nil {
		if stdin, ok := p.cmd.Stdin.(io.WriteCloser); ok {
			stdin.Close()
		}
		p.cmd.Process.Kill()
	}
}

func (p *AgentProcess) logConfigOptions(sessionID string, options []acp.ConfigOption) {
	if !p.debug {
		return
	}
	if len(options) == 0 {
		log.Printf("[DEBUG] Session %s created for agent %s (no configurable options available)", sessionID, p.name)
		return
	}
	log.Printf("[DEBUG] Session %s created for agent %s. Configurable options:", sessionID, p.name)
	for _, opt := range options {
		var details string
		if opt.Name != "" {
			details += fmt.Sprintf("name=%q", opt.Name)
		}
		if opt.Type != "" {
			if details != "" {
				details += ", "
			}
			details += fmt.Sprintf("type=%s", opt.Type)
		}
		if opt.CurrentValue != nil {
			if details != "" {
				details += ", "
			}
			details += fmt.Sprintf("current=%v", opt.CurrentValue)
		}
		if opt.Description != "" {
			if details != "" {
				details += ", "
			}
			details += fmt.Sprintf("desc=%q", opt.Description)
		}

		if details != "" {
			log.Printf("  - %s (%s)", opt.ID, details)
		} else {
			log.Printf("  - %s", opt.ID)
		}

		if len(opt.Options) > 0 {
			for _, o := range opt.Options {
				var oDetails string
				if o.Name != "" {
					oDetails += o.Name
				}
				if o.Description != "" {
					if oDetails != "" {
						oDetails += ": "
					}
					oDetails += o.Description
				}
				if oDetails != "" {
					log.Printf("    * %v (%s)", o.Value, oDetails)
				} else {
					log.Printf("    * %v", o.Value)
				}
			}
		}
	}
}
