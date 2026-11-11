package app

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Layout describes where everything lives on the USB stick.
//
//	<root>/
//	  Start-USB-AI.exe / Start-USB-AI.command / start-usb-ai.sh
//	  .system/bin/       launcher binaries for every OS
//	  .system/engines/   portable Ollama per OS/arch
//	  .system/home/      fake HOME for Ollama (keeps ~/.ollama off the host)
//	  models/            shared Ollama model store
//	  data/usbai.id      marker used to detect unplug / re-plug
//	  data/models.json   storage mode per model (pinned / swap / session)
//	  data/users/<id>/   encrypted per-user vaults
type Layout struct {
	Root    string
	System  string
	Engines string
	Home    string
	Models  string
	Data    string
	Users   string
	ID      string // unique id of this stick, stored in data/usbai.id
}

const markerFile = "usbai.id"

// NewLayout builds the layout for the given root, creating folders if needed.
func NewLayout(root string) (*Layout, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	l := &Layout{}
	l.setRoot(abs)
	for _, d := range []string{l.System, l.Engines, l.Home, l.Models, l.Data, l.Users} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	idPath := filepath.Join(l.Data, markerFile)
	b, err := os.ReadFile(idPath)
	if err != nil || len(strings.TrimSpace(string(b))) == 0 {
		buf := make([]byte, 16)
		_, _ = rand.Read(buf)
		id := hex.EncodeToString(buf)
		if err := os.WriteFile(idPath, []byte(id), 0o644); err != nil {
			return nil, err
		}
		b = []byte(id)
	}
	l.ID = strings.TrimSpace(string(b))
	return l, nil
}

func (l *Layout) setRoot(abs string) {
	l.Root = abs
	l.System = filepath.Join(abs, ".system")
	l.Engines = filepath.Join(l.System, "engines")
	l.Home = filepath.Join(l.System, "home")
	l.Models = filepath.Join(abs, "models")
	l.Data = filepath.Join(abs, "data")
	l.Users = filepath.Join(l.Data, "users")
}

// Present reports whether the USB (identified by its marker) is reachable.
func (l *Layout) Present() bool {
	b, err := os.ReadFile(filepath.Join(l.Data, markerFile))
	return err == nil && strings.TrimSpace(string(b)) == l.ID
}

// Relocate searches common mount points for this stick after it was re-plugged
// (it might come back under a different drive letter / mount path).
func (l *Layout) Relocate() bool {
	if l.Present() {
		return true
	}
	rel := filepath.Base(l.Root)
	for _, base := range mountCandidates() {
		for _, cand := range []string{base, filepath.Join(base, rel)} {
			b, err := os.ReadFile(filepath.Join(cand, "data", markerFile))
			if err == nil && strings.TrimSpace(string(b)) == l.ID {
				l.setRoot(cand)
				return true
			}
		}
	}
	return false
}

func mountCandidates() []string {
	var out []string
	switch runtime.GOOS {
	case "windows":
		for c := 'D'; c <= 'Z'; c++ {
			out = append(out, string(c)+`:\`)
		}
	case "darwin":
		ents, _ := os.ReadDir("/Volumes")
		for _, e := range ents {
			out = append(out, filepath.Join("/Volumes", e.Name()))
		}
	default:
		user := os.Getenv("USER")
		for _, base := range []string{"/media/" + user, "/run/media/" + user, "/media", "/mnt"} {
			ents, _ := os.ReadDir(base)
			for _, e := range ents {
				out = append(out, filepath.Join(base, e.Name()))
			}
		}
	}
	return out
}

// FindRoot walks up from the executable to find an existing USB-AI root
// (a folder containing .system or data/usbai.id). Falls back to exe dir.
func FindRoot(exeDir string) string {
	dir := exeDir
	for i := 0; i < 4; i++ {
		if fileExists(filepath.Join(dir, "data", markerFile)) || fileExists(filepath.Join(dir, ".system")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return exeDir
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// writeFileAtomic writes data via temp file + fsync + rename so a yanked USB
// never leaves a half-written vault behind.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		// Windows can fail renaming over an existing file on some FS (exFAT); retry.
		_ = os.Remove(path)
		if err2 := os.Rename(tmp, path); err2 != nil {
			return errors.Join(err, err2)
		}
	}
	return nil
}

// HostTempDir returns a RAM-backed temp dir where possible (Linux /dev/shm),
// otherwise the OS temp dir. Used for things that must survive an unplug.
func HostTempDir() string {
	if runtime.GOOS == "linux" {
		if st, err := os.Stat("/dev/shm"); err == nil && st.IsDir() {
			probe := filepath.Join("/dev/shm", ".usbai-probe")
			if err := os.WriteFile(probe, []byte("x"), 0o600); err == nil {
				os.Remove(probe)
				return "/dev/shm"
			}
		}
	}
	return os.TempDir()
}
