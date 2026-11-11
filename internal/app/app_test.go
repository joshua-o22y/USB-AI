package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVaultEncryptionAndUnplugRecovery(t *testing.T) {
	tmp := t.TempDir()
	layout, err := NewLayout(tmp)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	vm := NewVaultManager(layout)

	// 1. Create encrypted user vault
	v, err := vm.CreateUser("Joshua", "supersecret")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if v.Username != "Joshua" {
		t.Fatalf("unexpected username %q", v.Username)
	}

	// 2. Verify on-disk file does NOT contain plaintext
	encPath := filepath.Join(layout.Users, "joshua", "vault.enc")
	raw, err := os.ReadFile(encPath)
	if err != nil {
		t.Fatalf("ReadFile vault.enc: %v", err)
	}
	if string(raw) == "" || contains(string(raw), "Welcome to **USB-AI**") {
		t.Fatalf("vault.enc leaked plaintext!")
	}

	// 3. Logout and verify wrong password fails
	if err := vm.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := vm.Login("Joshua", "wrongpass"); err == nil {
		t.Fatalf("expected wrong password to fail")
	}

	// 4. Login with right password decrypts into RAM
	v2, err := vm.Login("Joshua", "supersecret")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if len(v2.Chats) != 1 {
		t.Fatalf("expected 1 welcome chat, got %d", len(v2.Chats))
	}

	// 5. Simulate USB yank by removing marker file, update in RAM, then restore marker
	marker := filepath.Join(layout.Data, markerFile)
	savedMarker, _ := os.ReadFile(marker)
	_ = os.Remove(marker)

	_, err = vm.Update(func(vd *VaultData) error {
		vd.Settings.SystemPrompt = "Updated while USB was unplugged"
		return nil
	})
	if err != nil {
		t.Fatalf("Update in RAM should succeed even when USB is unplugged: %v", err)
	}
	st := vm.Status()
	if st.USBConnected || !st.Dirty {
		t.Fatalf("expected USBConnected=false and Dirty=true while unplugged, got %+v", st)
	}

	// Plug USB back in -> Status() should auto-flush RAM to USB!
	_ = os.WriteFile(marker, savedMarker, 0o644)
	st2 := vm.Status()
	if !st2.USBConnected || st2.Dirty {
		t.Fatalf("expected auto-flush on re-plug, got %+v", st2)
	}
}

func TestHardwareRankingAndMoE(t *testing.T) {
	lowLaptop := Hardware{OS: "windows", Arch: "amd64", Cores: 4, RAMGB: 8, BestVRAMGB: 0, Tier: 1, TierName: "Featherweight"}
	ranked := RankModels(lowLaptop, map[string]string{"qwen3:4b": StoragePinned})
	if len(ranked) == 0 {
		t.Fatal("expected ranked models")
	}
	topCount := 0
	for _, m := range ranked {
		if m.Recommended {
			topCount++
		}
	}
	if topCount != 5 {
		t.Fatalf("expected 5 recommended models, got %d", topCount)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
