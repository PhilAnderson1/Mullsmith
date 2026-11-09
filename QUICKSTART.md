# Mullsmith quick start

Install Mullsmith, connect your model and send your first message. This walkthrough
is for using Mullsmith on a Linux computer or across a trusted home network.

If someone has already set up Mullsmith for you, open the address they gave you
and sign in with your account.

## Before you start

You will need:

- A Linux computer with administrator (`sudo`) access.
- A running model server, such as llama.cpp, Ollama or LM Studio, or access to a
  supported hosted provider such as OpenRouter. Mullsmith does not download or run
  the model itself.
- Your model server's API address, the exact model ID it serves, and an API key
  if it requires one.

The model server must be reachable from the computer running Mullsmith. If you
want to make Mullsmith accessible over the internet, follow the
[HTTPS setup](GUIDE.md#internet-access-and-https) before doing so.

## 1. Install Mullsmith

Download the package for your distribution and machine from
[Releases](https://github.com/PhilAnderson1/Mullsmith/releases). In the directory
where you downloaded it, run the command for your distribution.

**Debian or Ubuntu:**

```sh
sudo apt install ./mullsmith_*.deb
```

**RPM-based distributions:**

```sh
sudo dnf install ./mullsmith-*.rpm
```

Mullsmith starts automatically. For other installation methods, see
[tar installation](GUIDE.md#tar-archive) or
[building from source](GUIDE.md#building-from-source).

## 2. Create your account

Run this on the Mullsmith computer, replacing the example email, name and password
with your own. Choose a password of at least 10 characters.

```sh
sudo mullsmith admin create-user -email you@example.org -name 'Your name' -password 'your chosen password'
```

The default installation uses invite-only registration. This command creates your
account directly; you do not need an invitation or email setup.

## 3. Connect your model

Open `/etc/mullsmith/config.yaml` in a text editor with administrator access, for example:

```sh
sudo nano /etc/mullsmith/config.yaml
```

Find the existing `llm:` section. Update `endpoint`, `endpoint_type` and `api_key`
to match your model server. Replace the example `models:` list with your model's
details. This example connects to llama.cpp:

```yaml
llm:
  endpoint: "http://127.0.0.1:9931/v1"
  endpoint_type: "llama_cpp"
  api_key: ""
  models:
    - id: "your-model-id"
      default: true
      thinking_profile: "none"
      vision: false
```

Edit these entries within the existing section, keeping its other settings and
the rest of the file. Do not add a second `llm:` section. Use spaces, not tabs.

Replace `your-model-id` with the exact ID shown by your model server.
`127.0.0.1` means the model server runs on the same computer as Mullsmith; use its
network address if it runs elsewhere. Leave `api_key` empty only if no key is required.

For other servers, use the address and endpoint type in the
[model setup table](GUIDE.md#models-and-thinking-controls). That section also
explains how to enable image input and model-specific thinking controls later.

Save the file, then check your settings and connection:

```sh
sudo mullsmith -config /etc/mullsmith/config.yaml -check-config
```

Resolve any reported errors, then apply the settings:

```sh
sudo systemctl restart mullsmith
```

## 4. Send your first message

1. On your computer, tablet or phone, open `http://YOUR-SERVER-IP:3210`, replacing
   `YOUR-SERVER-IP` with the network IP address of the server running Mullsmith.
   Your device should be on the same network. If you are browsing on the
   Mullsmith server itself, you can use `http://localhost:3210` instead.
2. Sign in with the email and password you created above.
3. Start a new chat and send a simple message, such as “Give me three ideas for
   a quick lunch.”

When a reply appears and finishes, your basic setup is working. You can return
to the conversation later from the conversation list.

## If something does not work

| Problem | What to try |
|---|---|
| The page will not open | Run `sudo systemctl status mullsmith`. Check the browser address and, when connecting from another device, that your firewall allows access to port 3210 from your network. |
| You cannot sign in | Use the account created in step 2. Browser registration requires an invitation by default. |
| The page opens but you get no reply | Check that your model server is running, then run the configuration check in step 3. Verify the API address, model ID and any required API key. |
| Mullsmith will not restart after editing settings | Run the configuration check in step 3 and correct the reported errors. Check indentation and avoid duplicate sections. |

For more help, see [troubleshooting](GUIDE.md#troubleshooting).

## What next?

- [Enable tools](GUIDE.md#enable-tools) for web search and other capabilities.
- [Configure models and thinking controls](GUIDE.md#models-and-thinking-controls).
- [Invite other users](GUIDE.md#accounts-and-invitations).
- [Set up internet access with HTTPS](GUIDE.md#internet-access-and-https).
- [Back up your data](GUIDE.md#backups-and-storage-locations).
