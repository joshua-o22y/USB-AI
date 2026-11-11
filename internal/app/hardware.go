package app

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// GPU is a detected graphics adapter.
type GPU struct {
	Name    string  `json:"name"`
	Vendor  string  `json:"vendor"` // nvidia, amd, intel, apple, unknown
	VRAMGB  float64 `json:"vramGB"`
	Unified bool    `json:"unified"` // Apple Silicon / shared memory
}

// Hardware is the result of a host probe.
type Hardware struct {
	OS          string  `json:"os"`
	Arch        string  `json:"arch"`
	CPU         string  `json:"cpu"`
	Cores       int     `json:"cores"`
	RAMGB       float64 `json:"ramGB"`
	RAMFreeGB   float64 `json:"ramFreeGB"`
	GPUs        []GPU   `json:"gpus"`
	BestVRAMGB  float64 `json:"bestVramGB"` // usable accelerator memory
	Accelerator string  `json:"accelerator"`
	Tier        int     `json:"tier"` // 1 featherweight, 2 balanced, 3 powerhouse
	TierName    string  `json:"tierName"`
	ScannedAt   string  `json:"scannedAt"`
}

func run(timeout time.Duration, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ScanHardware probes CPU, RAM and GPUs without installing anything.
func ScanHardware() Hardware {
	h := Hardware{OS: runtime.GOOS, Arch: runtime.GOARCH, Cores: runtime.NumCPU(), ScannedAt: time.Now().Format(time.RFC3339)}
	switch runtime.GOOS {
	case "linux":
		scanLinux(&h)
	case "windows":
		scanWindows(&h)
	case "darwin":
		scanDarwin(&h)
	}
	if len(h.GPUs) == 0 || !hasNvidia(h.GPUs) {
		h.GPUs = append(h.GPUs, scanNvidiaSMI()...)
		h.GPUs = dedupeGPUs(h.GPUs)
	}
	classify(&h)
	return h
}

func hasNvidia(g []GPU) bool {
	for _, x := range g {
		if x.Vendor == "nvidia" && x.VRAMGB > 0 {
			return true
		}
	}
	return false
}

func dedupeGPUs(in []GPU) []GPU {
	seen := map[string]int{}
	var out []GPU
	for _, g := range in {
		k := strings.ToLower(g.Name)
		if i, ok := seen[k]; ok {
			if g.VRAMGB > out[i].VRAMGB {
				out[i] = g
			}
			continue
		}
		seen[k] = len(out)
		out = append(out, g)
	}
	return out
}

func scanNvidiaSMI() []GPU {
	bin := "nvidia-smi"
	if runtime.GOOS == "windows" {
		if p, err := exec.LookPath("nvidia-smi"); err == nil {
			bin = p
		} else if fileExists(`C:\Windows\System32\nvidia-smi.exe`) {
			bin = `C:\Windows\System32\nvidia-smi.exe`
		}
	}
	out := run(4*time.Second, bin, "--query-gpu=name,memory.total", "--format=csv,noheader,nounits")
	var gpus []GPU
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, ",")
		if len(parts) < 2 {
			continue
		}
		mb, _ := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		gpus = append(gpus, GPU{Name: strings.TrimSpace(parts[0]), Vendor: "nvidia", VRAMGB: round1(mb / 1024)})
	}
	return gpus
}

func scanLinux(h *Hardware) {
	if f, err := os.Open("/proc/cpuinfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "model name") {
				if i := strings.Index(line, ":"); i > 0 {
					h.CPU = strings.TrimSpace(line[i+1:])
					break
				}
			}
		}
		f.Close()
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			kb, _ := strconv.ParseFloat(f[1], 64)
			switch f[0] {
			case "MemTotal:":
				h.RAMGB = round1(kb / 1024 / 1024)
			case "MemAvailable:":
				h.RAMFreeGB = round1(kb / 1024 / 1024)
			}
		}
	}
	// AMD / Intel discrete via sysfs
	cards, _ := filepath.Glob("/sys/class/drm/card*/device")
	for _, dev := range cards {
		vendor, _ := os.ReadFile(filepath.Join(dev, "vendor"))
		v := strings.TrimSpace(string(vendor))
		vram, err := os.ReadFile(filepath.Join(dev, "mem_info_vram_total"))
		if err != nil {
			continue
		}
		bytes, _ := strconv.ParseFloat(strings.TrimSpace(string(vram)), 64)
		g := GPU{VRAMGB: round1(bytes / (1 << 30))}
		switch v {
		case "0x1002":
			g.Vendor, g.Name = "amd", "AMD Radeon GPU"
		case "0x8086":
			g.Vendor, g.Name = "intel", "Intel GPU"
		default:
			continue
		}
		if name := run(2*time.Second, "cat", filepath.Join(dev, "product_name")); name != "" {
			g.Name = name
		}
		// APUs report small carve-outs; ignore < 1 GB
		if g.VRAMGB >= 1 {
			h.GPUs = append(h.GPUs, g)
		}
	}
}

func scanWindows(h *Hardware) {
	ps := `$ErrorActionPreference='SilentlyContinue';` +
		`$c=Get-CimInstance Win32_Processor|Select-Object -First 1;` +
		`$s=Get-CimInstance Win32_ComputerSystem;$o=Get-CimInstance Win32_OperatingSystem;` +
		`"CPU|"+$c.Name;"RAM|"+$s.TotalPhysicalMemory;"FREE|"+($o.FreePhysicalMemory*1024);` +
		`Get-ItemProperty 'HKLM:\SYSTEM\ControlSet001\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}\0*'|` +
		`ForEach-Object{ if($_.DriverDesc){ "GPU|"+$_.DriverDesc+"|"+$_.'HardwareInformation.qwMemorySize'+"|"+$_.'HardwareInformation.MemorySize' } }`
	out := run(15*time.Second, "powershell", "-NoProfile", "-NonInteractive", "-Command", ps)
	for _, line := range strings.Split(out, "\n") {
		p := strings.Split(strings.TrimSpace(line), "|")
		if len(p) < 2 {
			continue
		}
		switch p[0] {
		case "CPU":
			h.CPU = strings.TrimSpace(p[1])
		case "RAM":
			v, _ := strconv.ParseFloat(p[1], 64)
			h.RAMGB = round1(v / (1 << 30))
		case "FREE":
			v, _ := strconv.ParseFloat(p[1], 64)
			h.RAMFreeGB = round1(v / (1 << 30))
		case "GPU":
			g := GPU{Name: p[1], Vendor: vendorOf(p[1])}
			var bytes float64
			if len(p) > 2 {
				bytes, _ = strconv.ParseFloat(p[2], 64)
			}
			if bytes == 0 && len(p) > 3 {
				bytes, _ = strconv.ParseFloat(p[3], 64)
			}
			g.VRAMGB = round1(bytes / (1 << 30))
			if g.Vendor == "intel" || strings.Contains(strings.ToLower(g.Name), "basic") {
				g.VRAMGB = 0 // integrated / shared
			}
			h.GPUs = append(h.GPUs, g)
		}
	}
}

func scanDarwin(h *Hardware) {
	h.CPU = run(3*time.Second, "sysctl", "-n", "machdep.cpu.brand_string")
	mem, _ := strconv.ParseFloat(run(3*time.Second, "sysctl", "-n", "hw.memsize"), 64)
	h.RAMGB = round1(mem / (1 << 30))
	// free = (free + inactive) pages from vm_stat
	vm := run(3*time.Second, "vm_stat")
	pageSize := 16384.0
	if ps, _ := strconv.ParseFloat(run(2*time.Second, "sysctl", "-n", "hw.pagesize"), 64); ps > 0 {
		pageSize = ps
	}
	var pages float64
	for _, line := range strings.Split(vm, "\n") {
		if strings.HasPrefix(line, "Pages free") || strings.HasPrefix(line, "Pages inactive") || strings.HasPrefix(line, "Pages speculative") {
			f := strings.Fields(strings.TrimSuffix(line, "."))
			v, _ := strconv.ParseFloat(strings.TrimSuffix(f[len(f)-1], "."), 64)
			pages += v
		}
	}
	h.RAMFreeGB = round1(pages * pageSize / (1 << 30))
	if runtime.GOARCH == "arm64" {
		// Apple Silicon: GPU shares memory; macOS lets the GPU wire ~70-75%.
		h.GPUs = append(h.GPUs, GPU{Name: "Apple Silicon GPU (" + h.CPU + ")", Vendor: "apple", VRAMGB: round1(h.RAMGB * 0.7), Unified: true})
	}
}

func vendorOf(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "nvidia") || strings.Contains(n, "geforce") || strings.Contains(n, "rtx") || strings.Contains(n, "quadro"):
		return "nvidia"
	case strings.Contains(n, "amd") || strings.Contains(n, "radeon"):
		return "amd"
	case strings.Contains(n, "intel"):
		return "intel"
	}
	return "unknown"
}

func classify(h *Hardware) {
	best := GPU{}
	for _, g := range h.GPUs {
		if g.VRAMGB > best.VRAMGB {
			best = g
		}
	}
	h.BestVRAMGB = best.VRAMGB
	switch {
	case best.Vendor == "apple":
		h.Accelerator = "Apple Metal (unified memory)"
	case best.Vendor == "nvidia" && best.VRAMGB > 0:
		h.Accelerator = "NVIDIA CUDA"
	case best.Vendor == "amd" && best.VRAMGB > 0:
		h.Accelerator = "AMD ROCm / Vulkan"
	default:
		h.Accelerator = "CPU only"
		h.BestVRAMGB = 0
	}
	switch {
	case h.BestVRAMGB >= 12 || (h.BestVRAMGB >= 8 && h.RAMGB >= 32):
		h.Tier, h.TierName = 3, "Powerhouse"
	case h.BestVRAMGB >= 4 || h.RAMGB >= 16:
		h.Tier, h.TierName = 2, "Balanced"
	default:
		h.Tier, h.TierName = 1, "Featherweight"
	}
	if h.CPU == "" {
		h.CPU = runtime.GOARCH + " CPU"
	}
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }
