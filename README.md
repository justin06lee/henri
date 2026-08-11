<p align="center">
  <img src="assets/henri-panel.svg" alt="henri" width="240">
</p>

<h1 align="center">henri</h1>

<p align="center">
  <em>A shared clipboard for all your devices.</em><br>
  Copy on one machine, paste on another. Encrypted, on your own network, no account, no cloud.
</p>

<p align="center">
  <img src="https://img.shields.io/badge/go-1.24%2B-00ADD8" alt="Go 1.24+">
  <img src="https://img.shields.io/badge/deps-none-brightgreen" alt="Zero dependencies">
  <img src="https://img.shields.io/badge/platforms-macOS%20%7C%20Linux%20%7C%20Windows-lightgrey" alt="Platforms">
</p>

---

## What it does

`henri` is a small daemon. It watches your clipboard, and the moment you copy
something it announces the change to your other devices and hands them the new
contents over an encrypted connection. They put it straight on their own
clipboard.

So: <kbd>⌘C</kbd> on the laptop, <kbd>Ctrl+V</kbd> on the desktop. That's the
whole idea.

- **One shared secret.** `henri init` on one device, `henri join <code>` on the
  rest. That code *is* the group key — there is no server and no account.
- **Encrypted end to end.** Every byte on the wire is sealed with AES-256-GCM
  under a key only your devices hold.
- **Finds your devices by itself.** Machines on the same network discover each
  other. Anything else you list by address.
- **Mixed groups work.** macOS, Linux and Windows devices all sit in the same
  group — the protocol is identical everywhere and text is normalised to UTF-8
  with `\n` line endings, so a copy on a Mac pastes unchanged on Linux.
- **One static binary, no dependencies.** Nothing outside the Go standard
  library, no cgo.

---

## Install

```sh
go install github.com/justin06lee/henri/cmd/henri@latest
```

Or build it from a clone:

```sh
git clone https://github.com/justin06lee/henri
cd henri
make build          # ./henri
sudo make install   # /usr/local/bin/henri
```

**Linux** additionally needs a clipboard helper — `wl-clipboard` on Wayland, or
`xclip`/`xsel` on X11:

```sh
sudo apt install wl-clipboard      # or: xclip
```

macOS and Windows work out of the box (`pbcopy`/`pbpaste`, PowerShell).

---

## Quick start

On your first device:

```console
$ henri init
Started a new clipboard group.

  config   /Users/you/.config/henri/config.json
  device   laptop
  group    BY_l6z_OVEA

Run this on every other device to join:

  henri join henri1:eyJnIjoiQllfbDZ6X09WRUEiLCJrIjoiWlIreWtDYzd4...
```

On every other device, paste that command:

```console
$ henri join henri1:eyJnIjoiQllfbDZ6X09WRUEiLCJrIjoiWlIreWtDYzd4...
Joined group BY_l6z_OVEA as "desktop".
```

Then start the daemon on each:

```sh
henri daemon
```

That's it. Copy something on one device and it is on the others' clipboards
before you can switch windows. To leave it running for good, see
[Running it for real](#running-it-for-real).

---

## Commands

| Command | What it does |
| --- | --- |
| `henri init` | Start a new clipboard group on this device |
| `henri join <code>` | Join an existing group |
| `henri code` | Print this group's join code again |
| `henri daemon` | Run the sync daemon in the foreground |
| `henri status` | Show what the local daemon is doing |
| `henri peers` | List known devices |
| `henri peers add <host:port>` | Add a device that discovery can't reach |
| `henri peers rm <host:port>` | Remove one |
| `henri send` | Re-send the current clipboard to the group |
| `henri version` | Print the version |

`henri status` is the one to reach for when something looks wrong:

```console
$ henri status
henri  ● running

  device     laptop  (dQw4w9WgXcQ)
  group      BY_l6z_OVEA
  clipboard  pbpaste
  listening  :47600   discovery on
  uptime     4h12m   pid 5512
  traffic    38 sent · 51 received
  last       1.2 KiB from desktop, 6s ago

peers
  ● desktop            192.168.1.42:47600     discovered  3s ago
  ● phone              192.168.1.77:47600     discovered  9s ago
  ○ 10.8.0.4:47600     10.8.0.4:47600         config      never
```

---

## How it works

```
   you press ⌘C
        │
        ▼
  ┌───────────────┐   polls every 400ms
  │    watcher    │   has the content changed?
  └───────┬───────┘
          │ yes
          ▼
  ┌──────────────────────────┐
  │  seal   AES-256-GCM      │
  │  key    HKDF(group key)  │
  └────────────┬─────────────┘
               │  TCP :47600
      ┌────────┼────────┐
      ▼        ▼        ▼
  ┌───────┐┌───────┐┌───────┐
  │desktop││ phone ││  vps  │
  └───────┘└───────┘└───────┘
   open · verify · write to the local clipboard

  discovery: every 10s each device multicasts an encrypted
  "hello" to 239.42.47.60:47601 so the others learn its address
```

A few details that matter:

**No echo storms.** Each device remembers the SHA-256 of the clipboard content
it currently considers in sync. A device claims that fingerprint *before* it
writes an incoming payload, so its own watcher sees the new content as
already-known and never bounces it back.

**Two keys, one secret.** The 32-byte group key in your config is never used
directly. HKDF-SHA256 derives one key for clipboard payloads and another for
discovery beacons, so a beacon can't be replayed as a payload. The group ID is
mixed in as additional authenticated data.

**Membership is the key.** There is no login. Sealing a message correctly is
what proves a device is in the group; anything that fails to authenticate is
dropped without a reply.

**Replays are refused.** Every message carries a timestamp and is rejected if
it's more than two minutes off. Payloads also carry a hash of their contents,
and a mismatch is refused.

---

## Configuration

`~/.config/henri/config.json` (override with `$HENRI_CONFIG` or
`$XDG_CONFIG_HOME`; `%APPDATA%\henri\` on Windows). It is written `0600` and
`henri` refuses to start if anyone else can read it — it holds your group key.

```json
{
  "group_id": "BY_l6z_OVEA",
  "key": "ZR+ykCc7xXwnDQkC55jkGw/n4gy66Bd1WPGcavgBvb8=",
  "device_id": "-wYRDywIF9A",
  "device_name": "laptop",
  "listen_port": 47600,
  "discovery_port": 47601,
  "discovery": true,
  "peers": ["10.8.0.4:47600"],
  "poll_interval_ms": 400,
  "max_payload_bytes": 4194304
}
```

| Key | Default | Notes |
| --- | --- | --- |
| `group_id` | generated | Identifies the group; shared by every device in it |
| `key` | generated | The 32-byte group secret, base64. **This is the credential.** |
| `device_id` | generated | Unique per device; not shared |
| `device_name` | hostname | What shows up in `henri peers` |
| `listen_port` | `47600` | TCP port that receives clipboard updates |
| `discovery_port` | `47601` | UDP port for multicast beacons |
| `discovery` | `true` | Set `false` to rely only on `peers` |
| `peers` | `[]` | Devices to always push to — for anything off the LAN |
| `poll_interval_ms` | `400` | How often the clipboard is checked |
| `max_payload_bytes` | `4194304` | Clipboards larger than this are skipped |

Devices that aren't on the same network — a VPS, or a laptop behind a different
router — won't be found by multicast. Add them by address:

```sh
henri peers add 10.8.0.4:47600
```

Over the open internet, put henri inside a WireGuard or Tailscale network rather
than forwarding port 47600. The payloads are encrypted either way, but there's
no reason to expose the listener.

### Mixing macOS and Linux

Nothing extra to configure — install henri on both, join them to the same group,
and they sync. Two things are worth knowing:

- **Linux needs a clipboard helper** (`wl-clipboard`, `xclip` or `xsel`), and it
  has to run inside your graphical session. A daemon started from a bare SSH
  shell has no clipboard to watch; `henri status` will show `clipboard  none`.
- **Open the port.** Many Linux distributions ship a firewall that drops inbound
  connections. Allow TCP 47600 and UDP 47601 from your LAN:

  ```sh
  sudo ufw allow from 192.168.1.0/24 to any port 47600 proto tcp
  sudo ufw allow from 192.168.1.0/24 to any port 47601 proto udp
  ```

If discovery does not find the other machine, `henri peers add <ip>:47600` on
both sides skips it entirely and tells you quickly whether the problem is
multicast or the firewall.

---

## Running it for real

**macOS** (launchd):

```sh
cp dist/com.justin06lee.henri.plist ~/Library/LaunchAgents/
launchctl load -w ~/Library/LaunchAgents/com.justin06lee.henri.plist
```

**Linux** (systemd user unit):

```sh
mkdir -p ~/.config/systemd/user
cp dist/henri.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now henri
journalctl --user -u henri -f
```

Both run as *your* user inside your graphical session, on purpose: the clipboard
belongs to that session, so a system-wide daemon could not reach it.

---

## Security

What henri gives you:

- Clipboard contents are encrypted and authenticated with AES-256-GCM. Nothing
  on your network can read or alter them.
- Only devices holding the group key can send or receive.
- The config file holding that key is `0600`, and henri refuses to run if it
  isn't.

What it does not give you, and you should know:

- **The join code is the key.** Anyone who gets it can read everything you copy,
  forever. Send it over something you trust, and treat it like a password.
- **There is no rotation yet.** To change the key you re-run `henri init` and
  re-join every device.
- **Your clipboard has your passwords in it.** That's true of any clipboard
  sync tool. Copying from a password manager will send that password to every
  device in the group.
- Discovery beacons are encrypted, but their *timing and size* are visible to
  anyone on your LAN — they can tell henri is running, not what you copied.

Found a problem? Open an issue.

---

## Limitations

- **Text only.** Images and files aren't synced yet.
- **Polling, not events.** The clipboard is checked every 400ms rather than
  subscribed to, which is portable but means copies register within about half a
  second rather than instantly.
- **Two daemons on one machine won't discover each other.** They share a
  clipboard and a multicast port, so only the first to start receives beacons.
  Not a problem in the real configuration — one daemon per device — but worth
  knowing if you're testing on a single box. Use `peers` for that.
- **No history.** henri syncs the current clipboard; it isn't a clipboard
  manager.

---

## Development

```sh
make test    # go test ./...
make race    # go test -race ./...
make vet
make dist    # cross-compile to dist/bin/
```

The tests run whole groups of nodes against a fake clipboard, so they never
touch the real one. They cover propagation between peers, the echo guard,
foreign-key rejection, replays, oversized payloads, and peer expiry.

```
cmd/henri            the CLI
internal/config      config file: load, save, validate
internal/secure      HKDF key derivation and AES-256-GCM
internal/clipboard   per-platform clipboard access
internal/node        the daemon: watcher, peers, discovery, protocol
assets/              the panel image, used for the README and the tray icon
```

---

## The name

Named after Henri from **Kindergarten WARS** — a manga by You Chiba, serialized
on Shōnen Jump+ since 2022, about a kindergarten staffed by retired
assassins. The image at the top is traced from a panel of him, and the same
panel is what henri uses for its taskbar icon.

No affiliation with the author or Shueisha. The artwork belongs to them — it's
here because I like the manga.
