# Install the CLI

Install `sproozi` on the machine where you control agent work. Agent sandboxes
run in Linux containers. Installing the CLI does not start a service or create
credentials.

## Homebrew

On macOS or Linux:

```sh
brew install andrewmccall/tap/sproozi
sproozi --version
```

The formula builds from the release source bundle. Homebrew installs Go as a
build dependency. To update:

```sh
brew update
brew upgrade sproozi
```

## Download a binary

Open the [latest release](https://github.com/andrewmccall/sproozi/releases/latest).
Choose the archive for your system:

| System | Architecture | Archive suffix |
| --- | --- | --- |
| macOS | Apple Silicon | `darwin_arm64.tar.gz` |
| macOS | Intel | `darwin_amd64.tar.gz` |
| Linux | x86-64 | `linux_amd64.tar.gz` |
| Linux | ARM64 | `linux_arm64.tar.gz` |
| Windows | x86-64 | `windows_amd64.zip` |

Download `checksums.txt` from the same release. Compare your archive's SHA-256
with its entry before extracting it. On macOS use `shasum -a 256 FILE`; on Linux
use `sha256sum FILE`. In PowerShell use:

```powershell
Get-FileHash .\sproozi_VERSION_windows_amd64.zip -Algorithm SHA256
Expand-Archive .\sproozi_VERSION_windows_amd64.zip -DestinationPath .\sproozi
.\sproozi\sproozi.exe --version
```

Replace `VERSION` with the release number. Put the extracted executable in a
directory on your `PATH`. Standalone binaries are unsigned. Homebrew is the
recommended macOS installation path.

## Choose the next step

The CLI controls sessions through the home daemon (`sproozi serve`) and remote
executors. These guides use it:

- [Local sessions](local-sessions.md) runs Codex or Claude Code in local Docker
  after `sproozi init --local`. It needs no cluster and no checkout.
- [Move a live session between Docker and Kubernetes](session-mobility.md)
  configures the home daemon and starts a session in local Docker.
- [Run a task remotely, then resume it locally](portable-workflows.md) runs Codex in
  Kubernetes and continues its work locally.
- [Hermes with remote work](retained-work-kubernetes.md) connects a persistent
  Hermes assistant to that remote work.
- On Windows, read [Use Sproozi on Windows](windows.md) before configuring the
  home daemon.

`sproozi init --local` pulls the release's worker image and prepares the
network, certificates and credentials. The other guides build images and the
remote executor from a checkout of this repository. Use the checkout of the
same release as your CLI: `sproozi --version` prints the version, and its tag
is that version with a `v` prefix, for example `git checkout v0.1.0`. The CLI
archive does not contain sandbox images.

The Kubernetes guides don't use the CLI. [Getting started](getting-started.md)
runs from a checkout with Make.
[Install on Kubernetes and run your first Pod](install-kubernetes.md) uses the
release's Kubernetes assets, which reference the same published images, or a
checkout.

## Build from source

With Go and Git installed, run this command in the repository:

```sh
go build -o bin/sproozi ./cmd/sproozi
```

On Windows use `-o bin/sproozi.exe`. Source builds report `dev` unless you set
release metadata with linker flags.
