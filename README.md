# USB-AI — Zero-Trace Portable Local AI Workspace

Plug a USB stick into any **Windows, macOS, or Linux** machine and launch a private, self-contained local AI environment that uses the host PC's hardware (CPU / RAM / GPU VRAM) while leaving **zero trace** on the host computer.

---

## Core Architecture & Features

1. **Zero-Trace Host RAM Session + AES-256-GCM Encrypted User Vaults**
   - Every user account on the USB (`data/users/<user>/vault.enc`) is encrypted with **AES-256-GCM** using a key derived from their login password via **PBKDF2-HMAC-SHA256 (210,000 iterations)**.
   - While unlocked, chats, settings, and documents live strictly in **Host RAM**—never on the host PC's hard drive.
   - **Smart USB Wear Protection & Unplug Recovery:** Instead of wearing out the USB flash memory with token-by-token writes, conversations stream in RAM and flush a single atomic checkpoint to the USB at the end of each response. If the USB stick is yanked early, an emergency alert prompts you to plug it back in and automatically flushes the RAM session upon reconnection.

2. **Dynamic Host Hardware Scanner + MoE & Hybrid RAM/VRAM Offloading**
   - Probes CPU cores, System RAM, and NVIDIA / AMD / Apple Silicon GPUs on every launch and classifies the host PC into **Tier 1 (Featherweight)**, **Tier 2 (Balanced)**, or **Tier 3 (Powerhouse)**.
   - Ranks the **Top 5 AI Models** for the current machine, highlighting **Mixture-of-Experts (MoE)** models (e.g., *Qwen 3 30B-A3B*, *DeepSeek V2 16B*, *Granite 3.1 MoE 3B*) that activate only a small fraction of parameters per word and split across **GPU VRAM + System RAM + USB `mmap` streaming**.

3. **3 Smart USB Storage Modes (Solves Limited USB Space)**
   - 📌 **Pinned to USB:** Stored permanently on the USB stick for 100% offline use anywhere.
   - 🔄 **Auto-Swap Slot:** Automatically replaces the previous Swap-Slot model when you download a new one so your USB never fills up.
   - ⚡ **Ephemeral Session:** Downloads and runs a model for the current session only and automatically deletes it on **Safe Eject**.

4. **3-Mode Privacy Network Firewall (`NetGuard`)**
   - 🔒 **Airlock Mode (Default):** 100% offline—blocks all outbound internet connections from both the launcher and Ollama (enforced via a local filtering proxy).
   - 📦 **Model Hub Mode:** Allows connections *only* to official Ollama / GitHub registries to download models or the portable engine.
   - 🌐 **Privacy Web Mode:** Enables optional per-message anonymized DuckDuckGo search snippets without sending your chat history to the cloud.

5. **Encrypted Knowledge Vault (Local RAG)**
   - Import `.txt`, `.md`, `.json`, `.csv`, or code files into your personal encrypted vault and ask questions about them offline.

---

## Building the Universal USB Stick (Windows + macOS + Linux)

```bash
chmod +x scripts/build-usb.sh
./scripts/build-usb.sh ./dist/USB-AI-Stick
```

Copy the contents of `./dist/USB-AI-Stick/` onto any **exFAT**-formatted USB drive:
- **Windows:** Double-click `Start-USB-AI.exe`
- **macOS:** Double-click `Start-USB-AI.command`
- **Linux:** Run `./start-usb-ai.sh`
