package app

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"sync"
	"time"
)

//go:embed ui/*
var uiFS embed.FS

// Server is the local-only HTTP server that serves the USB-AI UI and APIs.
type Server struct {
	layout        *Layout
	vaults        *VaultManager
	guard         *NetGuard
	ollama        *OllamaManager
	mu            sync.Mutex
	hw            Hardware
	simulateYank  bool
	ejected       bool
}

func NewServer(l *Layout) (*Server, error) {
	guard := NewNetGuard()
	proxyURL, err := guard.StartProxy()
	if err != nil {
		return nil, err
	}
	s := &Server{
		layout: l,
		vaults: NewVaultManager(l),
		guard:  guard,
		ollama: NewOllamaManager(l, guard, proxyURL),
		hw:     ScanHardware(),
	}
	// Background USB presence & periodic RAM-to-USB checkpoint loop (every 2 seconds).
	go s.watchdogLoop()
	return s, nil
}

func (s *Server) watchdogLoop() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for range t.C {
		s.mu.Lock()
		sim := s.simulateYank
		ejected := s.ejected
		s.mu.Unlock()
		if ejected || sim {
			continue
		}
		// Status() automatically detects re-plug and flushes dirty RAM state to USB.
		_ = s.vaults.Status()
	}
}

// Handler returns the HTTP router bound to localhost.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/bootstrap", s.handleBootstrap)
	mux.HandleFunc("/api/hardware/rescan", s.handleHardwareRescan)
	mux.HandleFunc("/api/auth/register", s.handleRegister)
	mux.HandleFunc("/api/auth/login", s.handleLogin)
	mux.HandleFunc("/api/auth/logout", s.handleLogout)
	mux.HandleFunc("/api/setup/complete", s.handleCompleteSetup)
	mux.HandleFunc("/api/settings", s.handleSettings)

	mux.HandleFunc("/api/models/pull", s.handleModelPull)
	mux.HandleFunc("/api/models/mode", s.handleModelMode)
	mux.HandleFunc("/api/models/delete", s.handleModelDelete)
	mux.HandleFunc("/api/engine/install", s.handleEngineInstall)
	mux.HandleFunc("/api/engine/start", s.handleEngineStart)

	mux.HandleFunc("/api/network/mode", s.handleNetworkMode)

	mux.HandleFunc("/api/chats/new", s.handleChatNew)
	mux.HandleFunc("/api/chats/delete", s.handleChatDelete)
	mux.HandleFunc("/api/chat/stream", s.handleChatStream)

	mux.HandleFunc("/api/vault/docs/add", s.handleDocAdd)
	mux.HandleFunc("/api/vault/docs/delete", s.handleDocDelete)

	mux.HandleFunc("/api/usb/status", s.handleUSBStatus)
	mux.HandleFunc("/api/usb/flush", s.handleUSBFlush)
	mux.HandleFunc("/api/usb/simulate-yank", s.handleSimulateYank)
	mux.HandleFunc("/api/usb/eject", s.handleSafeEject)

	sub, _ := fs.Sub(uiFS, "ui")
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return mux
}

func (s *Server) currentSyncStatus() SyncStatus {
	s.mu.Lock()
	sim := s.simulateYank
	ejected := s.ejected
	s.mu.Unlock()

	st := s.vaults.Status()
	if sim {
		st.USBConnected = false
		st.Dirty = true
		if st.PendingWrites == 0 {
			st.PendingWrites = 1
		}
		st.LastError = "⚠️ USB UNPLUGGED BEFORE SYNC! Your latest data is held in Host RAM. Plug the USB stick back in now or you will lose unsaved changes."
	}
	if ejected {
		st.LastError = "Safe Eject complete. Zero traces left on host PC — you may now remove the USB stick."
	}
	return st
}

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	users, _ := s.vaults.ListUsers()
	vault, loggedIn := s.vaults.Get()
	installed, modeMap := s.ollama.ListInstalled()

	s.mu.Lock()
	hw := s.hw
	ejected := s.ejected
	s.mu.Unlock()

	freeB, totalB := diskUsage(s.layout.Root)
	models := RankModels(hw, modeMap)

	writeJSON(w, http.StatusOK, map[string]any{
		"usbId":          s.layout.ID,
		"usbRoot":        s.layout.Root,
		"usbFreeGB":      round1(float64(freeB) / (1 << 30)),
		"usbTotalGB":     round1(float64(totalB) / (1 << 30)),
		"hardware":       hw,
		"users":          users,
		"loggedIn":       loggedIn,
		"vault":          vault,
		"models":         models,
		"installed":      installed,
		"pulls":          s.ollama.ActivePulls(),
		"engine":         s.ollama.Status(),
		"networkMode":    s.guard.Mode(),
		"networkLog":     s.guard.Log(),
		"sync":           s.currentSyncStatus(),
		"ejected":        ejected,
	})
}

func (s *Server) handleHardwareRescan(w http.ResponseWriter, r *http.Request) {
	hw := ScanHardware()
	s.mu.Lock()
	s.hw = hw
	s.mu.Unlock()
	_, modeMap := s.ollama.ListInstalled()
	writeJSON(w, http.StatusOK, map[string]any{
		"hardware": hw,
		"models":   RankModels(hw, modeMap),
	})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	vault, err := s.vaults.CreateUser(req.Username, req.Password)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// Newly registered users enter Setup Wizard; switch network to Model Hub so they can pull a model.
	_ = s.guard.SetMode(ModeHub)
	writeJSON(w, http.StatusOK, map[string]any{"vault": vault, "sync": s.currentSyncStatus(), "networkMode": s.guard.Mode()})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	vault, err := s.vaults.Login(req.Username, req.Password)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}
	if vault.Settings.PreferredNetMode != "" {
		_ = s.guard.SetMode(vault.Settings.PreferredNetMode)
	}
	writeJSON(w, http.StatusOK, map[string]any{"vault": vault, "sync": s.currentSyncStatus(), "networkMode": s.guard.Mode()})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.vaults.Logout(); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	_ = s.guard.SetMode(ModeAirlock)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleCompleteSetup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DefaultModel  string `json:"defaultModel"`
		StorageMode   string `json:"storageMode"`
		SystemPrompt  string `json:"systemPrompt"`
		HybridOffload bool   `json:"hybridOffload"`
		UseMMap       bool   `json:"useMMap"`
		PullNow       bool   `json:"pullNow"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.PullNow && req.DefaultModel != "" {
		if s.guard.Mode() == ModeAirlock {
			_ = s.guard.SetMode(ModeHub)
		}
		_ = s.ollama.PullModel(req.DefaultModel, req.StorageMode)
	}
	vault, err := s.vaults.Update(func(v *VaultData) error {
		if req.DefaultModel != "" {
			v.Settings.DefaultModel = req.DefaultModel
			if len(v.Chats) > 0 && v.Chats[0].Model == "" {
				v.Chats[0].Model = req.DefaultModel
			}
		}
		if req.SystemPrompt != "" {
			v.Settings.SystemPrompt = req.SystemPrompt
		}
		v.Settings.HybridOffload = req.HybridOffload
		v.Settings.UseMMap = req.UseMMap
		v.Settings.CompletedSetup = true
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// Return to strict Airlock mode once setup finishes (unless a download is actively running).
	if !req.PullNow {
		_ = s.guard.SetMode(ModeAirlock)
	}
	writeJSON(w, http.StatusOK, map[string]any{"vault": vault, "sync": s.currentSyncStatus(), "networkMode": s.guard.Mode()})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	var req UserSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	vault, err := s.vaults.Update(func(v *VaultData) error {
		v.Settings = req
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"vault": vault, "sync": s.currentSyncStatus()})
}

func (s *Server) handleModelPull(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model       string `json:"model"`
		StorageMode string `json:"storageMode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.ollama.PullModel(req.Model, req.StorageMode); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pulls": s.ollama.ActivePulls()})
}

func (s *Server) handleModelMode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model       string `json:"model"`
		StorageMode string `json:"storageMode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.ollama.SetStorageMode(req.Model, req.StorageMode); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	installed, modeMap := s.ollama.ListInstalled()
	writeJSON(w, http.StatusOK, map[string]any{
		"installed": installed,
		"models":    RankModels(s.hw, modeMap),
	})
}

func (s *Server) handleModelDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.ollama.DeleteModel(req.Model); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	installed, modeMap := s.ollama.ListInstalled()
	writeJSON(w, http.StatusOK, map[string]any{
		"installed": installed,
		"models":    RankModels(s.hw, modeMap),
	})
}

func (s *Server) handleEngineInstall(w http.ResponseWriter, r *http.Request) {
	if err := s.ollama.DownloadPortableEngine(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"engine": s.ollama.Status()})
}

func (s *Server) handleEngineStart(w http.ResponseWriter, r *http.Request) {
	if err := s.ollama.EnsureStarted(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"engine": s.ollama.Status()})
}

func (s *Server) handleNetworkMode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.guard.SetMode(req.Mode); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	_, _ = s.vaults.Update(func(v *VaultData) error {
		v.Settings.PreferredNetMode = req.Mode
		return nil
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"networkMode": s.guard.Mode(),
		"networkLog":  s.guard.Log(),
	})
}

func (s *Server) handleChatNew(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title string `json:"title"`
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	now := time.Now().Format(time.RFC3339)
	if req.Title == "" {
		req.Title = "New Conversation"
	}
	var created Chat
	vault, err := s.vaults.Update(func(v *VaultData) error {
		model := req.Model
		if model == "" {
			model = v.Settings.DefaultModel
		}
		created = Chat{
			ID:        randomHex(8),
			Title:     req.Title,
			Model:     model,
			CreatedAt: now,
			UpdatedAt: now,
			Messages:  []Message{},
		}
		v.Chats = append([]Chat{created}, v.Chats...)
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chat": created, "vault": vault, "sync": s.currentSyncStatus()})
}

func (s *Server) handleChatDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ChatID string `json:"chatId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	vault, err := s.vaults.Update(func(v *VaultData) error {
		var kept []Chat
		for _, c := range v.Chats {
			if c.ID != req.ChatID {
				kept = append(kept, c)
			}
		}
		v.Chats = kept
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"vault": vault, "sync": s.currentSyncStatus()})
}

func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ChatID    string `json:"chatId"`
		Model     string `json:"model"`
		Content   string `json:"content"`
		WebSearch bool   `json:"webSearch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	vaultSnap, ok := s.vaults.Get()
	if !ok {
		writeErr(w, http.StatusUnauthorized, fmt.Errorf("not logged in"))
		return
	}
	modelID := req.Model
	if modelID == "" {
		modelID = vaultSnap.Settings.DefaultModel
	}
	if modelID == "" {
		modelID = "qwen3:4b"
	}

	// Build prompt context in RAM (system instructions + Knowledge Vault RAG + optional Privacy Web Search).
	var sources []string
	sysPrompt := vaultSnap.Settings.SystemPrompt
	if vaultSnap.Settings.UseKnowledgeVault && len(vaultSnap.Docs) > 0 {
		if ragText, src := RetrieveVaultContext(vaultSnap.Docs, req.Content, 3000); ragText != "" {
			sysPrompt += "\n\n" + ragText
			sources = append(sources, src...)
		}
	}
	if req.WebSearch {
		if results, err := PrivacyWebSearch(s.guard, req.Content); err == nil && len(results) > 0 {
			sysPrompt += "\n\n[PRIVACY-PROXY ANONYMIZED WEB RESULTS]\n"
			for _, res := range results {
				sysPrompt += fmt.Sprintf("- %s (%s): %s\n", res.Title, res.URL, res.Snippet)
				sources = append(sources, "Web: "+res.Title)
			}
		}
	}

	now := time.Now().Format(time.RFC3339)
	userMsg := Message{Role: "user", Content: req.Content, CreatedAt: now}

	// Append user message in RAM first (without flushing every token—saves USB wear!).
	var history []Message
	if sysPrompt != "" {
		history = append(history, Message{Role: "system", Content: sysPrompt})
	}
	_, err := s.vaults.Update(func(v *VaultData) error {
		for i := range v.Chats {
			if v.Chats[i].ID == req.ChatID {
				v.Chats[i].Model = modelID
				if len(v.Chats[i].Messages) == 0 && (v.Chats[i].Title == "New Conversation" || v.Chats[i].Title == "Welcome to USB-AI") {
					title := req.Content
					if len(title) > 38 {
						title = title[:38] + "…"
					}
					v.Chats[i].Title = title
				}
				v.Chats[i].Messages = append(v.Chats[i].Messages, userMsg)
				v.Chats[i].UpdatedAt = now
				history = append(history, v.Chats[i].Messages...)
				return nil
			}
		}
		return fmt.Errorf("chat not found")
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	var fullReply string
	s.mu.Lock()
	hw := s.hw
	s.mu.Unlock()

	_ = s.ollama.StreamChat(r.Context(), modelID, history, vaultSnap.Settings, hw, func(tok string) {
		fullReply += tok
		b, _ := json.Marshal(map[string]any{"token": tok})
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	})

	// Single atomic encrypted write-back to USB at the end of the response!
	assistantMsg := Message{
		Role:      "assistant",
		Content:   fullReply,
		CreatedAt: time.Now().Format(time.RFC3339),
		Model:     modelID,
		Sources:   sources,
	}
	updatedVault, _ := s.vaults.Update(func(v *VaultData) error {
		for i := range v.Chats {
			if v.Chats[i].ID == req.ChatID {
				v.Chats[i].Messages = append(v.Chats[i].Messages, assistantMsg)
				v.Chats[i].UpdatedAt = assistantMsg.CreatedAt
				break
			}
		}
		return nil
	})
	donePayload, _ := json.Marshal(map[string]any{
		"done":    true,
		"message": assistantMsg,
		"vault":   updatedVault,
		"sync":    s.currentSyncStatus(),
	})
	fmt.Fprintf(w, "data: %s\n\n", donePayload)
	if flusher != nil {
		flusher.Flush()
	}
}

func (s *Server) handleDocAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" || req.Content == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("document name and content are required"))
		return
	}
	vault, err := s.vaults.Update(func(v *VaultData) error {
		doc := DocItem{
			ID:        randomHex(6),
			Name:      req.Name,
			Content:   req.Content,
			SizeBytes: len(req.Content),
			AddedAt:   time.Now().Format("2006-01-02 15:04"),
		}
		v.Docs = append([]DocItem{doc}, v.Docs...)
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"vault": vault, "sync": s.currentSyncStatus()})
}

func (s *Server) handleDocDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	vault, err := s.vaults.Update(func(v *VaultData) error {
		var kept []DocItem
		for _, d := range v.Docs {
			if d.ID != req.ID {
				kept = append(kept, d)
			}
		}
		v.Docs = kept
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"vault": vault, "sync": s.currentSyncStatus()})
}

func (s *Server) handleUSBStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"sync":        s.currentSyncStatus(),
		"pulls":       s.ollama.ActivePulls(),
		"engine":      s.ollama.Status(),
		"networkMode": s.guard.Mode(),
		"networkLog":  s.guard.Log(),
	})
}

func (s *Server) handleUSBFlush(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.simulateYank = false
	s.mu.Unlock()
	err := s.vaults.Flush()
	st := s.currentSyncStatus()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "sync": st})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sync": st})
}

func (s *Server) handleSimulateYank(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Unplugged bool `json:"unplugged"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.mu.Lock()
	s.simulateYank = req.Unplugged
	s.mu.Unlock()
	if !req.Unplugged {
		_ = s.vaults.Flush()
	}
	writeJSON(w, http.StatusOK, map[string]any{"sync": s.currentSyncStatus()})
}

func (s *Server) handleSafeEject(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.simulateYank = false
	s.mu.Unlock()

	if err := s.vaults.Flush(); err != nil {
		writeErr(w, http.StatusConflict, fmt.Errorf("cannot safely eject while USB is unreachable: %w", err))
		return
	}
	_ = s.vaults.Logout()
	s.ollama.PurgeEphemeralAndStop()
	_ = s.guard.SetMode(ModeAirlock)

	s.mu.Lock()
	s.ejected = true
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"ejected": true,
		"sync":    s.currentSyncStatus(),
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": err.Error()})
}
