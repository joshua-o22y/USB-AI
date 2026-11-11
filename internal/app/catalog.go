package app

import (
	"fmt"
	"math"
	"sort"
)

// ModelSpec describes an Ollama model in the built-in catalog, including
// Mixture-of-Experts (MoE) metadata and RAM/VRAM requirements.
type ModelSpec struct {
	ID             string   `json:"id"`             // Ollama tag, e.g. "qwen3:4b"
	Name           string   `json:"name"`           // Display name
	Family         string   `json:"family"`         // Qwen, Llama, DeepSeek, Gemma, Phi, Mistral
	Architecture   string   `json:"architecture"`   // "Dense" or "MoE" (Mixture of Experts)
	TotalParams    string   `json:"totalParams"`    // e.g. "30B" or "3.2B"
	ActiveParams   string   `json:"activeParams"`   // e.g. "3B active/token" for MoE
	ActiveParamsB  float64  `json:"activeParamsB"`  // numeric active billions per token
	DownloadSizeGB float64  `json:"downloadSizeGB"` // space on USB
	MinRAMGB       float64  `json:"minRamGB"`       // minimum RAM+VRAM to run (with mmap/offload)
	IdealVRAMGB    float64  `json:"idealVramGB"`    // fits 100% in GPU VRAM
	Capabilities   []string `json:"capabilities"`   // General, Coding, Reasoning, Multilingual, Fast
	Summary        string   `json:"summary"`
}

// ScoredModel pairs a catalog entry with live compatibility and ranking info
// for the currently connected host PC.
type ScoredModel struct {
	ModelSpec
	Score          int     `json:"score"`
	Rank           int     `json:"rank"`
	Recommended    bool    `json:"recommended"` // Top 5 for this PC
	FitBadge       string  `json:"fitBadge"`    // "fast", "hybrid", "playable", "heavy"
	FitLabel       string  `json:"fitLabel"`
	ExecutionPlan  string  `json:"executionPlan"` // e.g. "100% GPU VRAM" or "Hybrid: 4GB VRAM + 6GB RAM (MoE)"
	EstTokensPerS  int     `json:"estTokensPerSec"`
	Installed      bool    `json:"installed"`
	StorageMode    string  `json:"storageMode,omitempty"` // pinned | swap | session
}

// Catalog returns the curated list of Ollama models spanning tiny 1B laptop
// models, Mixture-of-Experts (MoE) sparse models, and 32B+ workstation models.
func Catalog() []ModelSpec {
	return []ModelSpec{
		{
			ID:             "llama3.2:1b",
			Name:           "Llama 3.2 1B Instruct",
			Family:         "Llama",
			Architecture:   "Dense",
			TotalParams:    "1.2B",
			ActiveParams:   "1.2B",
			ActiveParamsB:  1.2,
			DownloadSizeGB: 1.3,
			MinRAMGB:       2.5,
			IdealVRAMGB:    2.0,
			Capabilities:   []string{"Ultra-Fast", "Low RAM", "General"},
			Summary:        "Featherweight model that runs fast even on older 4GB–8GB laptops with no GPU.",
		},
		{
			ID:             "qwen2.5:1.5b",
			Name:           "Qwen 2.5 1.5B Instruct",
			Family:         "Qwen",
			Architecture:   "Dense",
			TotalParams:    "1.5B",
			ActiveParams:   "1.5B",
			ActiveParamsB:  1.5,
			DownloadSizeGB: 1.0,
			MinRAMGB:       2.5,
			IdealVRAMGB:    2.0,
			Capabilities:   []string{"Ultra-Fast", "Coding", "Multilingual"},
			Summary:        "Tiny footprint (~1GB on USB) with surprisingly strong coding and Q&A skills.",
		},
		{
			ID:             "llama3.2:3b",
			Name:           "Llama 3.2 3B Instruct",
			Family:         "Llama",
			Architecture:   "Dense",
			TotalParams:    "3.2B",
			ActiveParams:   "3.2B",
			ActiveParamsB:  3.2,
			DownloadSizeGB: 2.0,
			MinRAMGB:       4.5,
			IdealVRAMGB:    3.5,
			Capabilities:   []string{"General", "Summarization", "Laptop Sweet-Spot"},
			Summary:        "Meta's everyday 3B model—great balance of speed, low USB size, and smarts on any laptop.",
		},
		{
			ID:             "qwen3:4b",
			Name:           "Qwen 3 4B (Hybrid Thinking)",
			Family:         "Qwen",
			Architecture:   "Dense",
			TotalParams:    "4.0B",
			ActiveParams:   "4.0B",
			ActiveParamsB:  4.0,
			DownloadSizeGB: 2.6,
			MinRAMGB:       5.5,
			IdealVRAMGB:    4.0,
			Capabilities:   []string{"Reasoning", "Coding", "General"},
			Summary:        "Punches far above its weight class with step-by-step reasoning and fast CPU/GPU performance.",
		},
		{
			ID:             "gemma3:4b",
			Name:           "Gemma 3 4B Instruct",
			Family:         "Gemma",
			Architecture:   "Dense",
			TotalParams:    "4.3B",
			ActiveParams:   "4.3B",
			ActiveParamsB:  4.3,
			DownloadSizeGB: 3.3,
			MinRAMGB:       6.0,
			IdealVRAMGB:    4.5,
			Capabilities:   []string{"Creative Writing", "General", "Multilingual"},
			Summary:        "Google's compact open model with 128K context support and natural writing style.",
		},
		{
			ID:             "granite3.1-moe:3b",
			Name:           "Granite 3.1 MoE 3B (800M Active)",
			Family:         "Granite",
			Architecture:   "MoE",
			TotalParams:    "3.0B",
			ActiveParams:   "0.8B active/token",
			ActiveParamsB:  0.8,
			DownloadSizeGB: 2.0,
			MinRAMGB:       4.0,
			IdealVRAMGB:    3.0,
			Capabilities:   []string{"MoE Sparse", "Ultra-Fast", "RAG & Docs"},
			Summary:        "Mixture-of-Experts model that only activates 800M parameters per word—blazingly fast on CPU laptops!",
		},
		{
			ID:             "qwen2.5-coder:7b",
			Name:           "Qwen 2.5 Coder 7B",
			Family:         "Qwen",
			Architecture:   "Dense",
			TotalParams:    "7.6B",
			ActiveParams:   "7.6B",
			ActiveParamsB:  7.6,
			DownloadSizeGB: 4.7,
			MinRAMGB:       8.0,
			IdealVRAMGB:    6.0,
			Capabilities:   []string{"Coding", "Debugging", "Scripts"},
			Summary:        "Top-tier portable coding assistant for writing, refactoring, and explaining code offline.",
		},
		{
			ID:             "llama3.1:8b",
			Name:           "Llama 3.1 8B Instruct",
			Family:         "Llama",
			Architecture:   "Dense",
			TotalParams:    "8.0B",
			ActiveParams:   "8.0B",
			ActiveParamsB:  8.0,
			DownloadSizeGB: 4.9,
			MinRAMGB:       8.5,
			IdealVRAMGB:    6.5,
			Capabilities:   []string{"General", "Instruction Following", "Workhorse"},
			Summary:        "The gold-standard 8B all-rounder for 16GB laptops and mid-range gaming PCs.",
		},
		{
			ID:             "deepseek-r1:8b",
			Name:           "DeepSeek R1 Distill 8B",
			Family:         "DeepSeek",
			Architecture:   "Dense",
			TotalParams:    "8.0B",
			ActiveParams:   "8.0B",
			ActiveParamsB:  8.0,
			DownloadSizeGB: 4.9,
			MinRAMGB:       8.5,
			IdealVRAMGB:    6.5,
			Capabilities:   []string{"Deep Reasoning", "Math", "Logic"},
			Summary:        "Chain-of-thought reasoning model that thinks through tough logic, math, and code problems.",
		},
		{
			ID:             "deepseek-v2:16b",
			Name:           "DeepSeek V2 Lite 16B MoE (2.4B Active)",
			Family:         "DeepSeek",
			Architecture:   "MoE",
			TotalParams:    "15.7B",
			ActiveParams:   "2.4B active/token",
			ActiveParamsB:  2.4,
			DownloadSizeGB: 8.9,
			MinRAMGB:       11.0,
			IdealVRAMGB:    10.0,
			Capabilities:   []string{"MoE Sparse", "High IQ on Laptops", "RAM+VRAM Split"},
			Summary:        "16B brain that only runs 2.4B parameters per token! Ideal for splitting across GPU VRAM + System RAM.",
		},
		{
			ID:             "phi4:14b",
			Name:           "Microsoft Phi-4 14B",
			Family:         "Phi",
			Architecture:   "Dense",
			TotalParams:    "14.0B",
			ActiveParams:   "14.0B",
			ActiveParamsB:  14.0,
			DownloadSizeGB: 9.1,
			MinRAMGB:       13.0,
			IdealVRAMGB:    10.5,
			Capabilities:   []string{"STEM", "Reasoning", "Coding"},
			Summary:        "High-density 14B model trained on curated textbook-quality data.",
		},
		{
			ID:             "qwen3:30b-a3b",
			Name:           "Qwen 3 30B-A3B MoE (Only 3B Active!)",
			Family:         "Qwen",
			Architecture:   "MoE",
			TotalParams:    "30.5B",
			ActiveParams:   "3.0B active/token",
			ActiveParamsB:  3.0,
			DownloadSizeGB: 18.0,
			MinRAMGB:       16.0,
			IdealVRAMGB:    20.0,
			Capabilities:   []string{"MoE Flagship", "RAM+VRAM Hybrid", "30B IQ at 3B Speed"},
			Summary:        "Flagship Mixture-of-Experts: 30B total knowledge, but activates only 3B per word. Streams across VRAM + RAM!",
		},
		{
			ID:             "qwen2.5:32b",
			Name:           "Qwen 2.5 32B Instruct",
			Family:         "Qwen",
			Architecture:   "Dense",
			TotalParams:    "32.5B",
			ActiveParams:   "32.5B",
			ActiveParamsB:  32.5,
			DownloadSizeGB: 19.0,
			MinRAMGB:       24.0,
			IdealVRAMGB:    22.0,
			Capabilities:   []string{"Powerhouse", "Expert Coding", "Deep Analysis"},
			Summary:        "Workstation-grade dense 32B model for high-VRAM gaming rigs and 32GB+ machines.",
		},
	}
}

// RankModels evaluates every model against the detected host hardware and
// marks the Top 5 recommendations.
func RankModels(hw Hardware, installed map[string]string) []ScoredModel {
	cat := Catalog()
	ram := hw.RAMGB
	if ram <= 0 {
		ram = 8
	}
	vram := hw.BestVRAMGB
	effectiveMem := ram + vram
	// On unified memory (Apple Silicon), RAM and VRAM are the same pool.
	if len(hw.GPUs) > 0 && hw.GPUs[0].Unified {
		effectiveMem = ram
	}

	out := make([]ScoredModel, 0, len(cat))
	for _, m := range cat {
		sm := ScoredModel{ModelSpec: m}
		if mode, ok := installed[m.ID]; ok {
			sm.Installed = true
			sm.StorageMode = mode
		}

		// Determine how this model will execute on the current machine.
		switch {
		case vram >= m.IdealVRAMGB:
			sm.FitBadge = "fast"
			sm.FitLabel = "Fast on this PC (100% GPU VRAM)"
			sm.ExecutionPlan = fmt.Sprintf("100%% GPU VRAM (%.1f GB / %.1f GB VRAM)", m.IdealVRAMGB, vram)
			sm.EstTokensPerS = estimateTPS(m.ActiveParamsB, true, 1.0)

		case vram >= 2.0 && effectiveMem >= m.MinRAMGB:
			gpuShare := math.Min(0.85, vram/m.IdealVRAMGB)
			ramPart := math.Max(0.5, m.DownloadSizeGB-vram*0.85)
			sm.FitBadge = "hybrid"
			if m.Architecture == "MoE" {
				sm.FitLabel = "MoE Hybrid Split (VRAM + RAM)"
				sm.ExecutionPlan = fmt.Sprintf("Active experts (%s) in %.1f GB VRAM + %.1f GB System RAM", m.ActiveParams, vram, ramPart)
			} else {
				sm.FitLabel = "Hybrid GPU + RAM Split"
				sm.ExecutionPlan = fmt.Sprintf("Layers split: %.0f%% GPU VRAM + %.1f GB System RAM", gpuShare*100, ramPart)
			}
			sm.EstTokensPerS = estimateTPS(m.ActiveParamsB, true, gpuShare)

		case ram >= m.MinRAMGB:
			sm.FitBadge = "playable"
			if m.Architecture == "MoE" {
				sm.FitLabel = "Smart MoE on CPU/RAM"
				sm.ExecutionPlan = fmt.Sprintf("System RAM + USB mmap (%s active per word)", m.ActiveParams)
			} else {
				sm.FitLabel = "Runs in System RAM (CPU)"
				sm.ExecutionPlan = fmt.Sprintf("System RAM (%.1f GB required of %.1f GB)", m.MinRAMGB, ram)
			}
			sm.EstTokensPerS = estimateTPS(m.ActiveParamsB, false, 0)

		case ram+4.0 >= m.MinRAMGB && m.Architecture == "MoE":
			// MoE with mmap streaming from USB
			sm.FitBadge = "hybrid"
			sm.FitLabel = "USB mmap Stream + RAM (MoE)"
			sm.ExecutionPlan = fmt.Sprintf("Streams inactive experts from USB via mmap; %s active in RAM", m.ActiveParams)
			sm.EstTokensPerS = maxInt(4, estimateTPS(m.ActiveParamsB, false, 0)/2)

		default:
			sm.FitBadge = "heavy"
			sm.FitLabel = "Too heavy for this PC"
			sm.ExecutionPlan = fmt.Sprintf("Needs ~%.0f GB memory (this PC has %.1f GB RAM)", m.MinRAMGB, ram)
			sm.EstTokensPerS = 1
		}

		sm.Score = scoreForHost(sm, hw)
		out = append(out, sm)
	}

	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Score > out[j].Score
	})
	recCount := 0
	for i := range out {
		out[i].Rank = i + 1
		if out[i].FitBadge != "heavy" && recCount < 5 {
			out[i].Recommended = true
			recCount++
		}
	}
	return out
}

func scoreForHost(m ScoredModel, hw Hardware) int {
	if m.FitBadge == "heavy" {
		return 10 - int(m.DownloadSizeGB)
	}
	score := 50
	switch m.FitBadge {
	case "fast":
		score += 40
	case "hybrid":
		score += 32
	case "playable":
		score += 22
	}
	// Reward MoE models because they deliver big-model quality at small-model speed.
	if m.Architecture == "MoE" {
		score += 18
	}
	// Match model size to hardware tier so Tier 1 gets nimble models + MoE,
	// Tier 2 gets 4B-16B, Tier 3 gets 8B-32B.
	switch hw.Tier {
	case 1:
		if m.ActiveParamsB <= 3.5 {
			score += 25
		} else if m.ActiveParamsB <= 4.5 {
			score += 15
		} else {
			score -= int(m.ActiveParamsB * 3)
		}
	case 2:
		if m.ActiveParamsB >= 2.0 && m.ActiveParamsB <= 8.5 {
			score += 25
		}
	case 3:
		if m.DownloadSizeGB >= 4.5 {
			score += 25
		}
	}
	if m.Installed {
		score += 8
	}
	return score
}

func estimateTPS(activeB float64, hasGPU bool, gpuShare float64) int {
	if activeB <= 0 {
		activeB = 3
	}
	if hasGPU {
		base := 160.0 / math.Pow(activeB, 0.75)
		mult := 0.35 + 0.65*gpuShare
		return maxInt(6, int(base*mult))
	}
	base := 42.0 / math.Pow(activeB, 0.8)
	return maxInt(3, int(base))
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
