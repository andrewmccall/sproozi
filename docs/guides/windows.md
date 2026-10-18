# Use Sproozi on Windows

Install the x86-64 ZIP from [the release](install.md). Run commands in
PowerShell or Windows Terminal on Windows 11 or Windows Server 2025. You can submit work to configured remote owners
without running Docker locally. Local agent sessions require Docker Desktop
using Linux containers, plus the home configuration in the
[session mobility guide](session-mobility.md).

Native Windows support includes private files, process locks, local control,
Git workspace capture and terminal resize handling. CI tests these host
behaviours on Windows. A full Docker Desktop session and interactive console
check remain separate acceptance requirements. The Bash-based Kind demo and
cluster setup scripts require Linux, macOS or WSL2.

## Create private configuration

Run:

```powershell
sproozi init
```

This creates `%APPDATA%\sproozi` with private Windows permissions and prints its
path. It grants access to your account and SYSTEM. It rejects an existing
directory that grants access to other ordinary users. It does not change
permissions on existing files.

Save `config.json`, credentials and TLS keys inside that directory. Files
created there inherit its private permissions. Do not copy credentials into a
shared directory. Follow the session guide for the configuration fields and
required certificates.

Use absolute Windows paths in configuration. Escape backslashes in JSON:

```json
{
  "directory": "C:\\Users\\you\\AppData\\Roaming\\sproozi\\home",
  "dockerBinary": "C:\\Program Files\\Docker\\Docker\\resources\\bin\\docker.exe",
  "dockerSocket": "npipe:////./pipe/dockerDesktopLinuxEngine",
  "dockerConfig": "C:\\Users\\you\\AppData\\Roaming\\sproozi\\docker"
}
```

This shows the Windows-specific fields, not a complete home configuration.
Select Docker Desktop's Linux engine before starting the service. Sproozi uses
the configured local pipe and private Docker configuration. It does not use
your active Docker context or accept a remote Docker endpoint.

Private state must be on a local NTFS drive. Network shares, junctions and
other reparse points are rejected. The administrative connection is a private
named pipe derived from your account and home directory. Other local users
cannot control the service through it.

## Work with Git files

Install Git for Windows and keep `git.exe` on your `PATH`. Stop other writers
before starting or moving a workspace.

Sproozi preserves tracked executable-file flags from the Git index. New Windows
files enter the Linux workspace as ordinary non-executable files. To mark a
tracked script executable before capture, use:

```powershell
git update-index --chmod=+x script.sh
```

Windows cannot represent all Linux filenames or permissions. Sproozi rejects
reserved names, alternate data streams, trailing dots or spaces, and names
that collide when case is ignored. DOS short-name aliases are also rejected. Use ordinary directories, not linked
worktrees, junctions or symlinks.

Start the configured home with `sproozi serve --config PATH`. Use
`sproozi COMMAND --help` for command options. `Ctrl-]` detaches from an agent
terminal. Detaching leaves the managed session running.
