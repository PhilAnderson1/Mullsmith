# Mullsmith

[![CI](https://github.com/PhilAnderson1/Mullsmith/actions/workflows/ci.yml/badge.svg)](https://github.com/PhilAnderson1/Mullsmith/actions/workflows/ci.yml)

**A self-hosted web interface for AI models and tools.**

Run Mullsmith on your Linux computer or server, connect your models and tools,
and access them through a browser.

<p align="center">
  <img src="assets/mullsmith.png" alt="Mullsmith on desktop and phone, showing code execution, a downloadable file and web fetching" width="85%">
</p>

- **Go beyond chat.** Work with files and images, search the web, and let the
  assistant run code to calculate, process data and create downloadable results.
  Optional code execution is isolated and has approval controls.
- **Easy setup with thinking controls built in.** Included endpoint and model
  catalogues make setup straightforward, with reasoning controls suited to your model.
- **Connect more tools and services.** Connect local or remote MCP servers, with
  OAuth support for users to connect their own service accounts.
- **Keep accounts and history on your server.** Conversations and files stay on
  your server, with separate accounts for multiple users.
- **Responsive page design.** Access Mullsmith from your PC, tablet or mobile
  phone, or save it to your home screen as a web app.
- **Access from wherever you are.** Built-in HTTPS lets you serve Mullsmith
  directly over the internet, without setting up a reverse proxy.
- **Simple to install and configure.** One small, self-contained application
  with no separate database to manage.

## Get started

You need a Linux computer or server, administrator access to install Mullsmith,
and a running model server or supported hosted provider.

**[Quick start](QUICKSTART.md)** — install Mullsmith, connect your model and start chatting.

Mullsmith is intended for you and people you trust. For access over the internet,
follow the [HTTPS setup](GUIDE.md#internet-access-and-https) and keep registration
invite-only or closed.

Other installation options:
[Tar installation](GUIDE.md#tar-archive) ·
[Build from source](GUIDE.md#building-from-source)

## Documentation

- [Full guide](GUIDE.md) — setup, tools and everyday administration.
- [Models and thinking controls](GUIDE.md#models-and-thinking-controls) — connect
  your model and configure its capabilities.
- [MCP servers and OAuth](GUIDE.md#mcp-tool-servers) — add tools and let users
  connect their service accounts.
- [Troubleshooting](GUIDE.md#troubleshooting) — help when something does not work.
- [Configuration reference](configs/config.yaml) — detailed settings when you need them.
