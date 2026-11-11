package app

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const pbkdf2Iters = 210_000

// Message is one chat turn.
type Message struct {
	Role      string   `json:"role"` // system | user | assistant
	Content   string   `json:"content"`
	CreatedAt string   `json:"createdAt"`
	Model     string   `json:"model,omitempty"`
	Sources   []string `json:"sources,omitempty"`
}

// Chat is a conversation thread stored in a user's encrypted vault.
type Chat struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Model     string    `json:"model"`
	CreatedAt string    `json:"createdAt"`
	UpdatedAt string    `json:"updatedAt"`
	Messages  []Message `json:"messages"`
}

// DocItem is a document inside the user's encrypted Knowledge Vault (local RAG).
type DocItem struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Content   string `json:"content"`
	SizeBytes int    `json:"sizeBytes"`
	AddedAt   string `json:"addedAt"`
}

// UserSettings holds per-user preferences and system instructions.
type UserSettings struct {
	SystemPrompt       string  `json:"systemPrompt"`
	DefaultModel       string  `json:"defaultModel"`
	Temperature        float64 `json:"temperature"`
	ContextLength      int     `json:"contextLength"`
	HybridOffload      bool    `json:"hybridOffload"`
	UseMMap            bool    `json:"useMMap"`
	UseKnowledgeVault  bool    `json:"useKnowledgeVault"`
	CompletedSetup     bool    `json:"completedSetup"`
	PreferredNetMode   string  `json:"preferredNetMode"`
	SwapSlotBudgetGB   float64 `json:"swapSlotBudgetGB"`
}

// VaultData is the plaintext payload that lives ONLY in host RAM while unlocked
// and is encrypted with AES-256-GCM before being flushed to the USB stick.
type VaultData struct {
	Username  string       `json:"username"`
	CreatedAt string       `json:"createdAt"`
	UpdatedAt string       `json:"updatedAt"`
	Settings  UserSettings `json:"settings"`
	Chats     []Chat       `json:"chats"`
	Docs      []DocItem    `json:"docs"`
}

// vaultEnvelope is the on-disk JSON file stored at data/users/<id>/vault.enc.
type vaultEnvelope struct {
	Username   string `json:"username"`
	CreatedAt  string `json:"createdAt"`
	SaltHex    string `json:"salt"`
	NonceHex   string `json:"nonce"`
	CipherHex  string `json:"ciphertext"`
	KDF        string `json:"kdf"`
	Iterations int    `json:"iterations"`
}

// UserSummary is safe metadata shown on the login screen without unlocking.
type UserSummary struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	CreatedAt string `json:"createdAt"`
}

// SyncStatus reports the state of the RAM-to-USB checkpoint engine.
type SyncStatus struct {
	USBConnected  bool   `json:"usbConnected"`
	Dirty         bool   `json:"dirty"`
	PendingWrites int    `json:"pendingWrites"`
	LastSavedAt   string `json:"lastSavedAt"`
	LastError     string `json:"lastError,omitempty"`
	ActiveUser    string `json:"activeUser,omitempty"`
}

// VaultManager holds the active user's decrypted vault in RAM and flushes
// encrypted checkpoints to the USB stick.
type VaultManager struct {
	mu            sync.Mutex
	layout        *Layout
	activeID      string
	activeKey     []byte
	activeSalt    []byte
	ram           *VaultData
	dirty         bool
	pendingWrites int
	lastSavedAt   string
	lastError     string
}

func NewVaultManager(l *Layout) *VaultManager {
	return &VaultManager{layout: l}
}

var safeUserRe = regexp.MustCompile(`[^a-z0-9_-]+`)

func userID(username string) string {
	s := strings.ToLower(strings.TrimSpace(username))
	s = safeUserRe.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if s == "" {
		s = "user"
	}
	return s
}

func (vm *VaultManager) vaultPath(id string) string {
	return filepath.Join(vm.layout.Users, id, "vault.enc")
}

// ListUsers returns all accounts stored on the USB stick.
func (vm *VaultManager) ListUsers() ([]UserSummary, error) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	ents, err := os.ReadDir(vm.layout.Users)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []UserSummary
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(vm.layout.Users, e.Name(), "vault.enc")
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var env vaultEnvelope
		if json.Unmarshal(b, &env) != nil || env.Username == "" {
			continue
		}
		out = append(out, UserSummary{ID: e.Name(), Username: env.Username, CreatedAt: env.CreatedAt})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Username) < strings.ToLower(out[j].Username) })
	return out, nil
}

// CreateUser creates a new AES-256-GCM encrypted vault on the USB and unlocks it in RAM.
func (vm *VaultManager) CreateUser(username, password string) (*VaultData, error) {
	username = strings.TrimSpace(username)
	if len(username) < 2 {
		return nil, errors.New("username must be at least 2 characters")
	}
	if len(password) < 4 {
		return nil, errors.New("password must be at least 4 characters")
	}
	id := userID(username)

	vm.mu.Lock()
	defer vm.mu.Unlock()

	if fileExists(vm.vaultPath(id)) {
		return nil, errors.New("user already exists on this USB")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iters, 32)
	if err != nil {
		return nil, err
	}
	now := time.Now().Format(time.RFC3339)
	welcomeChat := Chat{
		ID:        randomHex(8),
		Title:     "Welcome to USB-AI",
		Model:     "",
		CreatedAt: now,
		UpdatedAt: now,
		Messages: []Message{
			{
				Role:      "assistant",
				Content:   "Welcome to **USB-AI**! Your workspace is running strictly in host RAM and encrypted with **AES-256-GCM** on your USB drive.\n\n- Pick a recommended model from the top bar or run the **Setup Wizard**.\n- Toggle **Airlock / Model Hub / Privacy Web** in the header at any time.\n- Add private notes or documents to **My Knowledge Vault** to chat with them locally.",
				CreatedAt: now,
			},
		},
	}
	data := &VaultData{
		Username:  username,
		CreatedAt: now,
		UpdatedAt: now,
		Settings: UserSettings{
			SystemPrompt:      "You are a helpful, privacy-first AI assistant running locally from an encrypted USB stick. Give clear, accurate, and concise answers.",
			DefaultModel:      "",
			Temperature:       0.7,
			ContextLength:     4096,
			HybridOffload:     true,
			UseMMap:           true,
			UseKnowledgeVault: true,
			CompletedSetup:    false,
			PreferredNetMode:  ModeAirlock,
			SwapSlotBudgetGB:  12,
		},
		Chats: []Chat{welcomeChat},
		Docs:  []DocItem{},
	}
	vm.activeID = id
	vm.activeKey = key
	vm.activeSalt = salt
	vm.ram = data
	vm.dirty = true
	vm.pendingWrites = 1
	if err := vm.flushLocked(); err != nil {
		return nil, err
	}
	return cloneVault(vm.ram), nil
}

// Login decrypts a user's vault into host RAM using their password.
func (vm *VaultManager) Login(usernameOrID, password string) (*VaultData, error) {
	id := userID(usernameOrID)

	vm.mu.Lock()
	defer vm.mu.Unlock()

	b, err := os.ReadFile(vm.vaultPath(id))
	if err != nil {
		return nil, errors.New("account not found on USB")
	}
	var env vaultEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, errors.New("corrupted vault envelope")
	}
	salt, err := hex.DecodeString(env.SaltHex)
	if err != nil {
		return nil, err
	}
	nonce, err := hex.DecodeString(env.NonceHex)
	if err != nil {
		return nil, err
	}
	ct, err := hex.DecodeString(env.CipherHex)
	if err != nil {
		return nil, err
	}
	iters := env.Iterations
	if iters <= 0 {
		iters = pbkdf2Iters
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, iters, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, errors.New("invalid password (decryption failed)")
	}
	var data VaultData
	if err := json.Unmarshal(plain, &data); err != nil {
		return nil, errors.New("corrupted vault data")
	}
	vm.activeID = id
	vm.activeKey = key
	vm.activeSalt = salt
	vm.ram = &data
	vm.dirty = false
	vm.pendingWrites = 0
	vm.lastError = ""
	vm.lastSavedAt = data.UpdatedAt
	return cloneVault(vm.ram), nil
}

// Logout flushes any pending RAM changes to USB and wipes the key from RAM.
func (vm *VaultManager) Logout() error {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.ram != nil && vm.dirty {
		if err := vm.flushLocked(); err != nil {
			return err
		}
	}
	for i := range vm.activeKey {
		vm.activeKey[i] = 0
	}
	vm.activeID = ""
	vm.activeKey = nil
	vm.activeSalt = nil
	vm.ram = nil
	vm.dirty = false
	vm.pendingWrites = 0
	return nil
}

// Get returns a snapshot of the active user's decrypted RAM vault.
func (vm *VaultManager) Get() (*VaultData, bool) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.ram == nil {
		return nil, false
	}
	return cloneVault(vm.ram), true
}

// Update mutates the in-RAM vault and attempts an immediate atomic checkpoint
// to the USB stick. If the USB was yanked, the update remains safely in RAM
// and Dirty stays true until the USB is plugged back in.
func (vm *VaultManager) Update(fn func(v *VaultData) error) (*VaultData, error) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.ram == nil {
		return nil, errors.New("not logged in")
	}
	if err := fn(vm.ram); err != nil {
		return nil, err
	}
	vm.ram.UpdatedAt = time.Now().Format(time.RFC3339)
	vm.dirty = true
	vm.pendingWrites++
	_ = vm.flushLocked()
	return cloneVault(vm.ram), nil
}

// Flush forces an encrypted checkpoint from RAM to the USB stick.
func (vm *VaultManager) Flush() error {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.ram == nil || !vm.dirty {
		return nil
	}
	return vm.flushLocked()
}

func (vm *VaultManager) flushLocked() error {
	if vm.ram == nil {
		return nil
	}
	if !vm.layout.Relocate() {
		vm.lastError = "USB stick disconnected! Plug the USB back in immediately to save your session data."
		return errors.New(vm.lastError)
	}
	plain, err := json.Marshal(vm.ram)
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(vm.activeKey)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	ct := gcm.Seal(nil, nonce, plain, nil)
	env := vaultEnvelope{
		Username:   vm.ram.Username,
		CreatedAt:  vm.ram.CreatedAt,
		SaltHex:    hex.EncodeToString(vm.activeSalt),
		NonceHex:   hex.EncodeToString(nonce),
		CipherHex:  hex.EncodeToString(ct),
		KDF:        "PBKDF2-HMAC-SHA256",
		Iterations: pbkdf2Iters,
	}
	out, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(vm.vaultPath(vm.activeID), out); err != nil {
		vm.lastError = "Failed writing to USB: " + err.Error()
		return err
	}
	vm.dirty = false
	vm.pendingWrites = 0
	vm.lastSavedAt = time.Now().Format("15:04:05")
	vm.lastError = ""
	return nil
}

// Status returns the current USB presence and RAM-sync status. If the USB was
// unplugged and just got plugged back in, it automatically flushes pending RAM changes!
func (vm *VaultManager) Status() SyncStatus {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	present := vm.layout.Relocate()
	if present && vm.dirty && vm.ram != nil {
		_ = vm.flushLocked()
	}
	if !present && vm.lastError == "" {
		vm.lastError = "USB stick not detected! Plug it back in so your RAM session can be saved."
	}
	user := ""
	if vm.ram != nil {
		user = vm.ram.Username
	}
	return SyncStatus{
		USBConnected:  present,
		Dirty:         vm.dirty,
		PendingWrites: vm.pendingWrites,
		LastSavedAt:   vm.lastSavedAt,
		LastError:     vm.lastError,
		ActiveUser:    user,
	}
}

func cloneVault(v *VaultData) *VaultData {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	var out VaultData
	_ = json.Unmarshal(b, &out)
	return &out
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
