package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/joshua-o22y/USB-AI/internal/app"
)

func main() {
	var (
		rootFlag   = flag.String("root", "", "USB root directory (defaults to folder containing the launcher)")
		addrFlag   = flag.String("addr", "127.0.0.1:0", "Bind address (defaults to random localhost port)")
		noBrowser  = flag.Bool("no-browser", false, "Do not auto-open browser window on startup")
	)
	flag.Parse()

	root := *rootFlag
	if root == "" {
		exe, err := os.Executable()
		if err == nil {
			root = app.FindRoot(filepath.Dir(exe))
		} else {
			wd, _ := os.Getwd()
			root = wd
		}
	}

	layout, err := app.NewLayout(root)
	if err != nil {
		log.Fatalf("USB-AI failed to initialize USB layout at %s: %v", root, err)
	}

	srv, err := app.NewServer(layout)
	if err != nil {
		log.Fatalf("USB-AI failed to start server: %v", err)
	}

	ln, err := net.Listen("tcp", *addrFlag)
	if err != nil {
		log.Fatalf("USB-AI failed to listen on %s: %v", *addrFlag, err)
	}
	url := "http://" + ln.Addr().String()

	fmt.Println("================================================================")
	fmt.Println("  USB-AI — Zero-Trace Portable Local AI Workspace")
	fmt.Println("================================================================")
	fmt.Printf("  USB Root       : %s\n", layout.Root)
	fmt.Printf("  USB Stick ID   : %s\n", layout.ID)
	fmt.Printf("  Local UI URL   : %s\n", url)
	fmt.Println("  Privacy Mode   : Host RAM Session + AES-256-GCM Vaults")
	fmt.Println("================================================================")

	if !*noBrowser {
		go openIsolatedBrowser(url)
	}

	// Cleanly flush RAM vault and purge ephemeral models on Ctrl+C / SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n[USB-AI] Shutting down — flushing encrypted RAM vault to USB & wiping session...")
		_, _ = http.Get(url + "/api/usb/eject")
		os.Exit(0)
	}()

	if err := http.Serve(ln, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}

func openIsolatedBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		if _, err := exec.LookPath("xdg-open"); err == nil {
			cmd = exec.Command("xdg-open", url)
		}
	}
	if cmd != nil {
		_ = cmd.Start()
	}
}
