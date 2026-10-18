# Install the CLI

Install `sproozi` on the machine where you control agent work. Agent workers run
in Linux containers. Installing the CLI does not start a service or create
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

- To try Kubernetes locally, follow [getting started](getting-started.md).
- To use an existing cluster, follow [Kubernetes installation](install-kubernetes.md).
- To move sessions between Docker and Kubernetes, follow [session mobility](session-mobility.md).
- On Windows, read [Windows setup](windows.md) before configuring a home service.

The Kubernetes and session guides include the required Linux images, network
setup and credentials. The CLI archive does not contain worker images.

## Build from source

With Go and Git installed, run this command in the repository:

```sh
go build -o bin/sproozi ./cmd/sproozi
```

On Windows use `-o bin/sproozi.exe`. Source builds report `dev` unless you set
release metadata with linker flags.
