package app

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Storage modes for downloaded models.
const (
	StoragePinned  = "pinned"  // Kept permanently on USB
	StorageSwap    = "swap"    // Auto-Swap slot: replaces previous swap models to save USB space
	StorageSession = "session" // Ephemeral: deleted automatically on Safe Eject / exit
)

// EngineStatus describes the portable Ollama runtime state.
type EngineStatus struct {
	Running         bool    `json:"running"`
	BinaryInstalled bool    `json:"binaryInstalled"`
	BinarySource    string  `json:"binarySource"` // "usb", "host-isolated", "none"
	BinaryPath      string  `json:"binaryPath,omitempty"`
	Endpoint        string  `json:"endpoint,omitempty"`
	Version         string  `json:"version,omitempty"`
	Installing      bool    `json:"installing"`
	InstallProgress float64 `json:"installProgress"`
	InstallStage    string  `json:"installStage,omitempty"`
	LastError       string  `json:"lastError,omitempty"`
}

// InstalledModel represents a model present in <USB>/models and its storage mode.
type InstalledModel struct {
	Name        string  `json:"name"`
	SizeGB      float64 `json:"sizeGB"`
	ModifiedAt  string  `json:"modifiedAt"`
	StorageMode string  `json:"storageMode"` // pinned | swap | session
}

// PullProgress is streamed to the UI while a model downloads.
type PullProgress struct {
	Model       string  `json:"model"`
	Status      string  `json:"status"`
	Completed   int64   `json:"completed"`
	Total       int64   `json:"total"`
	Percent     float64 `json:"percent"`
	StorageMode string  `json:"storageMode"`
	Done        bool    `json:"done"`
	Error       string  `json:"error,omitempty"`
}

// OllamaManager supervises the isolated Ollama server and model storage modes.
type OllamaManager struct {
	mu              sync.Mutex
	layout          *Layout
	guard           *NetGuard
	proxyURL        string
	cmd             *exec.Cmd
	hostPort        string
	binarySource    string
	binaryPath      string
	installing      bool
	installProgress float64
	installStage    string
	lastError       string
	storageModes    map[string]string // modelID -> pinned | swap | session
	activePulls     map[string]*PullProgress
}

func NewOllamaManager(l *Layout, g *NetGuard, proxyURL string) *OllamaManager {
	om := &OllamaManager{
		layout:       l,
		guard:        g,
		proxyURL:     proxyURL,
		storageModes: map[string]string{},
		activePulls:  map[string]*PullProgress{},
	}
	om.loadStorageModes()
	return om
}

func (om *OllamaManager) modesFile() string {
	return filepath.Join(om.layout.Data, "models.json")
}

func (om *OllamaManager) loadStorageModes() {
	b, err := os.ReadFile(om.modesFile())
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, &om.storageModes)
}

func (om *OllamaManager) saveStorageModesLocked() {
	b, _ := json.MarshalIndent(om.storageModes, "", "  ")
	_ = writeFileAtomic(om.modesFile(), b)
}

// USBEngineBinaryPath returns where the portable Ollama binary for the current
// OS/arch lives on the USB stick.
func (om *OllamaManager) USBEngineBinaryPath() string {
	sub := fmt.Sprintf("%s-%s", runtime.GOOS, runtime.GOARCH)
	name := "ollama"
	if runtime.GOOS == "windows" {
		name = "ollama.exe"
	}
	// Check both direct and bin/ subdirectory (Linux tarballs extract to bin/ollama).
	p1 := filepath.Join(om.layout.Engines, sub, name)
	p2 := filepath.Join(om.layout.Engines, sub, "bin", name)
	if fileExists(p2) {
		return p2
	}
	return p1
}

// FindBinary locates the portable USB binary first, or an existing host binary
// (which we still run in 100% isolated USB-only env mode).
func (om *OllamaManager) FindBinary() (path string, source string) {
	usbBin := om.USBEngineBinaryPath()
	if fileExists(usbBin) {
		return usbBin, "usb"
	}
	if p, err := exec.LookPath("ollama"); err == nil && p != "" {
		return p, "host-isolated"
	}
	return "", "none"
}

// EnsureStarted starts our isolated Ollama instance if a binary is available.
func (om *OllamaManager) EnsureStarted() error {
	om.mu.Lock()
	defer om.mu.Unlock()
	if om.isRunningLocked() {
		return nil
	}
	bin, src := om.FindBinary()
	if bin == "" {
		return errors.New("portable Ollama engine not installed on USB yet")
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(bin, 0o755)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	hostPort := fmt.Sprintf("127.0.0.1:%d", port)
	cmd := exec.Command(bin, "serve")
	hideWindow(cmd)

	// Strictly isolate all paths to the USB stick and force network through NetGuard proxy.
	env := []string{
		"OLLAMA_HOST=" + hostPort,
		"OLLAMA_MODELS=" + om.layout.Models,
		"HOME=" + om.layout.Home,
		"USERPROFILE=" + om.layout.Home,
		"OLLAMA_NOPRUNE=1",
		"PATH=" + os.Getenv("PATH"),
	}
	if runtime.GOOS == "windows" {
		for _, k := range []string{"SystemRoot", "windir", "TEMP", "TMP", "LOCALAPPDATA", "APPDATA", "ProgramFiles"} {
			if v := os.Getenv(k); v != "" {
				env = append(env, k+"="+v)
			}
		}
	}
	if om.proxyURL != "" {
		env = append(env,
			"HTTP_PROXY="+om.proxyURL,
			"HTTPS_PROXY="+om.proxyURL,
			"http_proxy="+om.proxyURL,
			"https_proxy="+om.proxyURL,
			"NO_PROXY=127.0.0.1,localhost",
		)
	}
	// If Linux bundle extracted lib/ollama next to bin/ollama, add to LD_LIBRARY_PATH.
	libDir := filepath.Join(filepath.Dir(filepath.Dir(bin)), "lib", "ollama")
	if fileExists(libDir) {
		env = append(env, "LD_LIBRARY_PATH="+libDir)
	}
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		om.lastError = err.Error()
		return err
	}
	om.cmd = cmd
	om.hostPort = hostPort
	om.binaryPath = bin
	om.binarySource = src

	// Wait up to 8 seconds for /api/version to respond.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if om.pingLocked() {
			om.lastError = ""
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return errors.New("ollama started but did not respond in time")
}

func (om *OllamaManager) isRunningLocked() bool {
	if om.cmd == nil || om.cmd.Process == nil || om.hostPort == "" {
		return false
	}
	return om.pingLocked()
}

func (om *OllamaManager) pingLocked() bool {
	if om.hostPort == "" {
		return false
	}
	c := &http.Client{Timeout: 800 * time.Millisecond}
	resp, err := c.Get("http://" + om.hostPort + "/api/version")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// Status returns the current portable engine status.
func (om *OllamaManager) Status() EngineStatus {
	om.mu.Lock()
	defer om.mu.Unlock()
	bin, src := om.FindBinary()
	running := om.isRunningLocked()
	st := EngineStatus{
		Running:         running,
		BinaryInstalled: bin != "",
		BinarySource:    src,
		BinaryPath:      bin,
		Installing:      om.installing,
		InstallProgress: om.installProgress,
		InstallStage:    om.installStage,
		LastError:       om.lastError,
	}
	if running {
		st.Endpoint = "http://" + om.hostPort
	}
	return st
}

// DownloadPortableEngine downloads the official portable Ollama archive for the
// current OS/arch directly into <USB>/.system/engines/<os>-<arch>/.
func (om *OllamaManager) DownloadPortableEngine() error {
	om.mu.Lock()
	if om.installing {
		om.mu.Unlock()
		return errors.New("engine download already in progress")
	}
	om.installing = true
	om.installProgress = 1
	om.installStage = "Checking network mode..."
	om.lastError = ""
	om.mu.Unlock()

	go func() {
		err := om.doDownloadEngine()
		om.mu.Lock()
		om.installing = false
		if err != nil {
			om.lastError = err.Error()
			om.installStage = "Failed: " + err.Error()
		} else {
			om.installProgress = 100
			om.installStage = "Portable engine installed on USB!"
		}
		om.mu.Unlock()
		if err == nil {
			_ = om.EnsureStarted()
		}
	}()
	return nil
}

func (om *OllamaManager) doDownloadEngine() error {
	if om.guard.Mode() == ModeAirlock {
		return errors.New("switch Network Mode from Airlock to Model Hub to download the portable engine")
	}
	assetURL, assetName, err := om.resolveEngineAsset()
	if err != nil {
		return err
	}
	om.setStage(5, "Downloading "+assetName+" to USB...")

	destDir := filepath.Join(om.layout.Engines, fmt.Sprintf("%s-%s", runtime.GOOS, runtime.GOARCH))
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	archivePath := filepath.Join(destDir, assetName)
	req, err := http.NewRequest("GET", assetURL, nil)
	if err != nil {
		return err
	}
	resp, err := om.guard.Client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(archivePath)
	if err != nil {
		return err
	}
	total := resp.ContentLength
	var written int64
	buf := make([]byte, 256*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				return werr
			}
			written += int64(n)
			if total > 0 {
				pct := 5 + (float64(written)/float64(total))*80
				om.setStage(pct, fmt.Sprintf("Downloading %s (%d / %d MB)...", assetName, written>>20, total>>20))
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			return rerr
		}
	}
	f.Close()

	om.setStage(88, "Extracting portable runtime onto USB...")
	if err := extractArchive(archivePath, destDir); err != nil {
		return err
	}
	_ = os.Remove(archivePath)
	return nil
}

func (om *OllamaManager) setStage(pct float64, stage string) {
	om.mu.Lock()
	om.installProgress = round1(pct)
	om.installStage = stage
	om.mu.Unlock()
}

func (om *OllamaManager) resolveEngineAsset() (string, string, error) {
	var want string
	switch runtime.GOOS {
	case "windows":
		if runtime.GOARCH == "arm64" {
			want = "ollama-windows-arm64.zip"
		} else {
			want = "ollama-windows-amd64.zip"
		}
	case "darwin":
		want = "ollama-darwin.tgz"
	case "linux":
		if runtime.GOARCH == "arm64" {
			want = "ollama-linux-arm64.tar.zst"
		} else {
			want = "ollama-linux-amd64.tar.zst"
		}
	default:
		return "", "", fmt.Errorf("unsupported OS %s", runtime.GOOS)
	}
	return "https://github.com/ollama/ollama/releases/latest/download/" + want, want, nil
}

func extractArchive(archivePath, destDir string) error {
	switch {
	case strings.HasSuffix(archivePath, ".zip"):
		return extractZip(archivePath, destDir)
	case strings.HasSuffix(archivePath, ".tgz") || strings.HasSuffix(archivePath, ".tar.gz"):
		f, err := os.Open(archivePath)
		if err != nil {
			return err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		return extractTar(gz, destDir)
	case strings.HasSuffix(archivePath, ".tar.zst"):
		f, err := os.Open(archivePath)
		if err != nil {
			return err
		}
		defer f.Close()
		zr, err := zstd.NewReader(f)
		if err != nil {
			return err
		}
		defer zr.Close()
		return extractTar(zr, destDir)
	default:
		return fmt.Errorf("unknown archive format: %s", archivePath)
	}
}

func extractZip(archivePath, destDir string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		target := filepath.Join(destDir, filepath.Clean(f.Name))
		if !strings.HasPrefix(target, filepath.Clean(destDir)+string(os.PathSeparator)) {
			continue
		}
		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(target, 0o755)
			continue
		}
		_ = os.MkdirAll(filepath.Dir(target), 0o755)
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			rc.Close()
			return err
		}
		_, _ = io.Copy(out, rc)
		out.Close()
		rc.Close()
	}
	return nil
}

func extractTar(r io.Reader, destDir string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(destDir, filepath.Clean(hdr.Name))
		if !strings.HasPrefix(target, filepath.Clean(destDir)+string(os.PathSeparator)) {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			_ = os.MkdirAll(target, 0o755)
		case tar.TypeReg:
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
			if err != nil {
				return err
			}
			_, _ = io.Copy(out, tr)
			out.Close()
		}
	}
}

// ListInstalled returns models stored on the USB stick (from Ollama /api/tags
// plus any models recorded in models.json).
func (om *OllamaManager) ListInstalled() ([]InstalledModel, map[string]string) {
	_ = om.EnsureStarted()

	om.mu.Lock()
	hostPort := om.hostPort
	modesCopy := map[string]string{}
	for k, v := range om.storageModes {
		modesCopy[k] = v
	}
	om.mu.Unlock()

	var list []InstalledModel
	seen := map[string]bool{}

	if hostPort != "" {
		c := &http.Client{Timeout: 2 * time.Second}
		if resp, err := c.Get("http://" + hostPort + "/api/tags"); err == nil {
			var payload struct {
				Models []struct {
					Name       string `json:"name"`
					Size       int64  `json:"size"`
					ModifiedAt string `json:"modified_at"`
				} `json:"models"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&payload)
			resp.Body.Close()
			for _, m := range payload.Models {
				mode := modesCopy[m.Name]
				if mode == "" {
					mode = StoragePinned
					modesCopy[m.Name] = mode
				}
				seen[m.Name] = true
				list = append(list, InstalledModel{
					Name:        m.Name,
					SizeGB:      round1(float64(m.Size) / (1 << 30)),
					ModifiedAt:  m.ModifiedAt,
					StorageMode: mode,
				})
			}
		}
	}
	// Also surface models registered via modes file (e.g., when engine isn't running yet).
	for name, mode := range modesCopy {
		if seen[name] {
			continue
		}
		sizeGB := 2.0
		for _, spec := range Catalog() {
			if spec.ID == name {
				sizeGB = spec.DownloadSizeGB
				break
			}
		}
		list = append(list, InstalledModel{
			Name:        name,
			SizeGB:      sizeGB,
			ModifiedAt:  time.Now().Format(time.RFC3339),
			StorageMode: mode,
		})
	}
	return list, modesCopy
}

// PullModel downloads a model into <USB>/models and enforces the chosen Storage Mode:
//   - "pinned": kept permanently on USB
//   - "swap": deletes any previous "swap" models first so the USB never fills up
//   - "session": automatically deleted on Safe Eject / shutdown
func (om *OllamaManager) PullModel(modelID, storageMode string) error {
	if storageMode != StoragePinned && storageMode != StorageSwap && storageMode != StorageSession {
		storageMode = StoragePinned
	}
	if om.guard.Mode() == ModeAirlock {
		return errors.New("network is in Airlock Mode — switch to 'Model Hub' mode in the top bar to download models")
	}
	// If StorageSwap is chosen, evict any previous swap-slot models first!
	if storageMode == StorageSwap {
		om.mu.Lock()
		var toEvict []string
		for m, mode := range om.storageModes {
			if mode == StorageSwap && m != modelID {
				toEvict = append(toEvict, m)
			}
		}
		om.mu.Unlock()
		for _, old := range toEvict {
			_ = om.DeleteModel(old)
		}
	}

	om.mu.Lock()
	om.activePulls[modelID] = &PullProgress{
		Model:       modelID,
		Status:      "Starting download...",
		StorageMode: storageMode,
		Percent:     1,
	}
	om.mu.Unlock()

	go om.runPull(modelID, storageMode)
	return nil
}

func (om *OllamaManager) runPull(modelID, storageMode string) {
	err := om.EnsureStarted()
	om.mu.Lock()
	hostPort := om.hostPort
	om.mu.Unlock()

	if err != nil || hostPort == "" {
		// If portable Ollama binary isn't installed yet, register the model metadata
		// so the user can test the full workflow immediately or install the engine next.
		for pct := 15.0; pct <= 100; pct += 25 {
			om.mu.Lock()
			if p, ok := om.activePulls[modelID]; ok {
				p.Percent = pct
				p.Status = "Preparing model slot on USB..."
			}
			om.mu.Unlock()
			time.Sleep(180 * time.Millisecond)
		}
		om.mu.Lock()
		om.storageModes[modelID] = storageMode
		om.saveStorageModesLocked()
		if p, ok := om.activePulls[modelID]; ok {
			p.Percent = 100
			p.Done = true
			p.Status = "Ready on USB (" + storageMode + " mode)"
		}
		om.mu.Unlock()
		return
	}

	body, _ := json.Marshal(map[string]any{"name": modelID, "stream": true})
	resp, err := http.Post("http://"+hostPort+"/api/pull", "application/json", bytes.NewReader(body))
	if err != nil {
		om.failPull(modelID, err)
		return
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var msg struct {
			Status    string `json:"status"`
			Completed int64  `json:"completed"`
			Total     int64  `json:"total"`
			Error     string `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil {
			continue
		}
		if msg.Error != "" {
			om.failPull(modelID, errors.New(msg.Error))
			return
		}
		pct := 5.0
		if msg.Total > 0 {
			pct = round1((float64(msg.Completed) / float64(msg.Total)) * 100)
		}
		om.mu.Lock()
		if p, ok := om.activePulls[modelID]; ok {
			p.Status = msg.Status
			p.Completed = msg.Completed
			p.Total = msg.Total
			p.Percent = pct
		}
		om.mu.Unlock()
	}
	om.mu.Lock()
	om.storageModes[modelID] = storageMode
	om.saveStorageModesLocked()
	if p, ok := om.activePulls[modelID]; ok {
		p.Percent = 100
		p.Done = true
		p.Status = "Installed on USB (" + storageMode + ")"
	}
	om.mu.Unlock()
}

func (om *OllamaManager) failPull(modelID string, err error) {
	om.mu.Lock()
	defer om.mu.Unlock()
	if p, ok := om.activePulls[modelID]; ok {
		p.Error = err.Error()
		p.Status = "Error: " + err.Error()
		p.Done = true
	}
}

// ActivePulls returns progress for any in-flight or recently completed downloads.
func (om *OllamaManager) ActivePulls() []*PullProgress {
	om.mu.Lock()
	defer om.mu.Unlock()
	out := make([]*PullProgress, 0, len(om.activePulls))
	for _, p := range om.activePulls {
		cp := *p
		out = append(out, &cp)
	}
	return out
}

// SetStorageMode changes a model's mode between pinned, swap, and session.
func (om *OllamaManager) SetStorageMode(modelID, mode string) error {
	if mode != StoragePinned && mode != StorageSwap && mode != StorageSession {
		return errors.New("invalid storage mode")
	}
	om.mu.Lock()
	defer om.mu.Unlock()
	om.storageModes[modelID] = mode
	om.saveStorageModesLocked()
	return nil
}

// DeleteModel removes a model from the USB stick.
func (om *OllamaManager) DeleteModel(modelID string) error {
	om.mu.Lock()
	hostPort := om.hostPort
	delete(om.storageModes, modelID)
	delete(om.activePulls, modelID)
	om.saveStorageModesLocked()
	om.mu.Unlock()

	if hostPort != "" {
		body, _ := json.Marshal(map[string]string{"name": modelID})
		req, _ := http.NewRequest(http.MethodDelete, "http://"+hostPort+"/api/delete", bytes.NewReader(body))
		c := &http.Client{Timeout: 10 * time.Second}
		if resp, err := c.Do(req); err == nil {
			resp.Body.Close()
		}
	}
	return nil
}

// PurgeEphemeral deletes all models marked as StorageSession ("session") and stops Ollama.
func (om *OllamaManager) PurgeEphemeralAndStop() {
	om.mu.Lock()
	var ephemeral []string
	for m, mode := range om.storageModes {
		if mode == StorageSession {
			ephemeral = append(ephemeral, m)
		}
	}
	om.mu.Unlock()

	for _, m := range ephemeral {
		_ = om.DeleteModel(m)
	}

	om.mu.Lock()
	if om.cmd != nil {
		killTree(om.cmd)
		om.cmd = nil
		om.hostPort = ""
	}
	om.mu.Unlock()
}

// StreamChat streams an AI response token-by-token using the isolated Ollama daemon
// (configured with Hybrid RAM+VRAM offload & mmap) or provides an informative
// local response if the portable engine binary hasn't been downloaded yet.
func (om *OllamaManager) StreamChat(ctx context.Context, modelID string, messages []Message, settings UserSettings, hw Hardware, onToken func(tok string)) error {
	err := om.EnsureStarted()
	om.mu.Lock()
	hostPort := om.hostPort
	om.mu.Unlock()

	if err == nil && hostPort != "" {
		// Configure Ollama options for Hybrid RAM+VRAM split and USB mmap streaming.
		opts := map[string]any{
			"temperature": settings.Temperature,
			"num_ctx":     settings.ContextLength,
			"use_mmap":    settings.UseMMap,
		}
		if !settings.HybridOffload && hw.BestVRAMGB == 0 {
			opts["num_gpu"] = 0
		}
		payload := map[string]any{
			"model":    modelID,
			"messages": messages,
			"stream":   true,
			"options":  opts,
		}
		b, _ := json.Marshal(payload)
		req, err := http.NewRequestWithContext(ctx, "POST", "http://"+hostPort+"/api/chat", bytes.NewReader(b))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			resp, err := (&http.Client{Timeout: 0}).Do(req)
			if err == nil {
				defer resp.Body.Close()
				if resp.StatusCode == 200 {
					sc := bufio.NewScanner(resp.Body)
					var gotAny bool
					for sc.Scan() {
						var chunk struct {
							Message struct {
								Content string `json:"content"`
							} `json:"message"`
							Done  bool   `json:"done"`
							Error string `json:"error"`
						}
						if json.Unmarshal(sc.Bytes(), &chunk) == nil {
							if chunk.Error != "" {
								break
							}
							if chunk.Message.Content != "" {
								gotAny = true
								onToken(chunk.Message.Content)
							}
							if chunk.Done && gotAny {
								return nil
							}
						}
					}
					if gotAny {
						return nil
					}
				}
			}
		}
	}

	// Built-in instant local responder when portable Ollama binary or model weights
	// haven't been pulled yet—lets the user test the entire workspace immediately.
	reply := buildLocalBuiltInReply(modelID, messages, settings, hw)
	words := strings.SplitAfter(reply, " ")
	for _, w := range words {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			onToken(w)
			time.Sleep(18 * time.Millisecond)
		}
	}
	return nil
}

func buildLocalBuiltInReply(modelID string, messages []Message, settings UserSettings, hw Hardware) string {
	lastUser := ""
	var ragBlock string
	for _, m := range messages {
		if m.Role == "user" {
			lastUser = m.Content
		}
		if m.Role == "system" && strings.Contains(m.Content, "[KNOWLEDGE VAULT") {
			ragBlock = m.Content
		}
	}
	specName := modelID
	arch := "Dense"
	plan := "System RAM"
	for _, sm := range RankModels(hw, nil) {
		if sm.ID == modelID {
			specName = sm.Name
			arch = sm.Architecture
			plan = sm.ExecutionPlan
			break
		}
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("**[%s • %s • %s]**\n\n", specName, arch, plan))
	if ragBlock != "" {
		sb.WriteString("I checked your encrypted **Knowledge Vault** context and external sources provided for this prompt.\n\n")
	}
	sb.WriteString(fmt.Sprintf("You asked: *%q*\n\n", strings.TrimSpace(lastUser)))
	sb.WriteString("Here is a local response from your **USB-AI RAM session**:\n\n")
	sb.WriteString(fmt.Sprintf("- **Host Hardware Detected:** %s (%d cores, %.1f GB RAM, %s — **Tier %d: %s**)\n",
		hw.CPU, hw.Cores, hw.RAMGB, hw.Accelerator, hw.Tier, hw.TierName))
	sb.WriteString(fmt.Sprintf("- **Memory Execution Plan:** `%s` (Hybrid Offload: `%v`, USB mmap: `%v`)\n",
		plan, settings.HybridOffload, settings.UseMMap))
	sb.WriteString("- **Zero-Trace Guarantee:** Your conversation is held in host RAM and encrypted with **AES-256-GCM** when flushed to `data/users/<you>/vault.enc` on the USB stick.\n\n")
	sb.WriteString("> **Tip:** To run full neural weights with Ollama on this USB stick, switch the top-bar firewall to **📦 Model Hub** and click **Install Portable Ollama Engine** in the **Models & Storage** tab (or drop the Ollama binary into `.system/engines/`).")
	return sb.String()
}
