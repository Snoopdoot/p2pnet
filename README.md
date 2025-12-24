# P2PNet

Local network P2P file sharing with a dark GUI. Works on Windows and Linux.

## Features

- Auto-discovers peers on your local network
- Share files with one click
- Dark theme GUI
- No server, no accounts, no bullshit

## Requirements

**To run:** Nothing - just the binary

**To build:**
- Go 1.21+
- GCC (Linux) or MinGW-w64 (Windows cross-compile)

## Build

```bash
# Linux
make linux

# Windows (from Linux)
sudo pacman -S mingw-w64-gcc   # Arch
make windows
```

## Usage

1. Run `./p2pnet` (or `p2pnet.exe` on Windows)
2. Click "Share File" to share files
3. Select a peer from the list
4. Click "Request File" and enter the filename

Peers on the same network find each other automatically.

## Ports

- UDP 42069 - Peer discovery (broadcast)
- TCP 42070 - File transfers

## License

MIT
