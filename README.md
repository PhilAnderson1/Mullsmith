# Mullsmith

**Your local AI assistant, on every device.**

A lightweight web interface for your model server, with built-in web and file
tools and optional sandboxed code execution. Use it from your PC, tablet or phone.

<p align="center">
  <img src="assets/mullsmith.png" alt="Mullsmith on desktop and phone, showing code execution, a downloadable file and web fetching" width="85%">
</p>

- **Small and self-contained.** Approximately 14 MiB. No Docker, Python, Node.js
  or separate database required for the core application.
- **Made for local models.** Connect to llama.cpp, Ollama, vLLM or LM Studio,
  with supplied model-specific thinking controls.
- **Useful tools included.** Search the web, work with files and download the results.
- **Share your server.** Separate accounts, private conversations and personal file storage.
- **Reach it anywhere.** Responsive, installable web app; built-in HTTPS for access
  beyond your home network.
- **Free and open source.** No subscriptions, paid tiers or upselling.

## Install

On Debian or Ubuntu, download the `.deb` from
[Releases](https://github.com/PhilAnderson1/Mullsmith/releases), then:

```sh
sudo apt install ./mullsmith_*.deb
```

**Open `http://YOUR-SERVER-IP:3210` from any device on your home network.**

Set your endpoint and model under `llm:` in `/etc/mullsmith/config.yaml`, then restart:

```sh
sudo systemctl restart mullsmith
```

[Other installation options](GUIDE.md#other-installation-options) ·
[Configuration reference](configs/config.yaml) ·
[Internet access and HTTPS](GUIDE.md#internet-access-and-https) ·
[Full guide](GUIDE.md)

Requires a running model endpoint. Optional code execution requires bubblewrap;
web search requires a Brave Search API key.
Enable HTTPS and review access settings before exposing the server to the internet.
