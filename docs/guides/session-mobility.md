# Move a live session between Docker and Kubernetes

Use this guide to move a session you are working in, interactively, between
local Docker and an approved Kubernetes environment and back again. You use the
agent's native terminal throughout. The harness conversation ID (`threadId`) and supported Git
workspace state stay the same, and the home daemon on your machine stays the
owner wherever the session runs.

A move keeps the execution's original deadline and total usage allowance. Each
destination gets fresh identity and access. To hand a task to Kubernetes, let it
finish there, and later take it over on your machine with a new deadline and
allowance, use
[Run a task remotely, then resume it locally](portable-workflows.md) instead.

**This setup needs a provider API key**, for OpenAI with Codex or Anthropic
with Claude Code. The local gateway does not use a ChatGPT or Claude plan
sign-in. You put the key in a file in
[Configure the local home](#configure-the-local-home).

This setup uses the Codex and Claude Code releases pinned in
[`hack/clients`](../../hack/clients/README.md). The
[recorded round trips](../../test/acceptance/reports/verified-session-mobility.md)
used earlier releases, Codex 0.161.0 and Claude Code 2.1.27, and haven't been
repeated with the current pins. A session keeps one harness throughout its
lifetime. Start a new managed
conversation; importing a personal agent home or adopting an existing running
session is not supported.

For local sessions only, `sproozi init --local` prepares everything in this
section; see [local sessions](local-sessions.md). Use the steps below when you
also need a remote executor or want to manage the configuration by hand.

## Prepare the administrator-owned setup

The trusted home daemon owns a local Unix Docker socket, fixed image IDs, its TLS
gateway, provider credentials and protected journals. Local start requires no
cluster or kubeconfig. A developer can use the remote executor after an
administrator provisions it. The remote service holds its own Kubernetes
credentials; the developer receives only the profile URL, public CA and protected
[home principal token](../../GLOSSARY.md#home-principal-token) file.

You need a running Linux Docker engine or Docker Desktop, Git, Go 1.26 or later,
and administrator-created images/network/TLS material. Keep all
service configuration and credentials outside the repository being transferred.
Portable state contains the selected conversation and workspace, including any
ordinary untracked files. It excludes home credentials, gateway tokens, provider
keys, Kubernetes credentials, private TLS keys and Git hooks/config. Protect the
workspace contents you choose to transfer; an untracked credential file would be
workspace data, so move it outside the source before capture.

[Install the CLI](install.md) on the developer's machine. It provides
`sproozi serve`, the home daemon, and the session commands below.

The remote executor and the native image have no release download, so
build them from a checkout of this repository that matches your CLI. Run
`sproozi --version`. Release binaries print a version, so check out its tag,
for example `git checkout v0.1.0`. A source build prints `dev` and a commit, so
build the other components from that commit (`git checkout <commit>`). If it
prints `source build`, use the checkout you built from. The home daemon,
remote executor and native supervisor reject messages they don't recognise, so
mixing releases can fail. From the repository root:

```sh
go build -o bin/session-profile ./cmd/session-profile
```

The native image is built in three layers.
[`hack/demo/codex.Dockerfile`](../../hack/demo/codex.Dockerfile) installs the
Codex release pinned in [`hack/clients`](../../hack/clients/README.md). The
[combined client image](../../internal/runtime/docker/image/clients.Dockerfile)
adds the pinned Claude Code release. The
[final native image](../../internal/runtime/docker/image/Dockerfile) adds the
fixed native supervisor, checkpoint helper and network anchor executable, and
checks that both clients are installed, so build all three. The same image
provides the separate native, helper and anchor containers, with commands and
privileges fixed by each role.

For sessions on one machine, build the images into the local Docker engine. No
registry is needed. `SPROOZI_IMAGE_PREFIX` names the local tags; give each
builder that shares a Docker engine its own prefix so their tags don't collide.
From the repository root:

```sh
SPROOZI_IMAGE_PREFIX=sproozi-local
docker build -f hack/demo/codex.Dockerfile -t "$SPROOZI_IMAGE_PREFIX/clients:codex" .
docker build -f internal/runtime/docker/image/clients.Dockerfile \
  --build-arg CODEX_CLIENTS_IMAGE="$SPROOZI_IMAGE_PREFIX/clients:codex" \
  -t "$SPROOZI_IMAGE_PREFIX/clients:latest" .
docker build -f internal/runtime/docker/image/Dockerfile \
  --build-arg CLIENTS_IMAGE="$SPROOZI_IMAGE_PREFIX/clients:latest" \
  -t "$SPROOZI_IMAGE_PREFIX/native:latest" .
SPROOZI_NATIVE_IMAGE_ID=$(docker image inspect --format '{{.Id}}' "$SPROOZI_IMAGE_PREFIX/native:latest")
echo "$SPROOZI_NATIVE_IMAGE_ID"
```

The first build compiles Git from source, so it is slow. The last command
prints the image's `sha256:...` content ID. The last two builds take their base
image only from the `--build-arg`, which is required: without it, the build
stops with `Set --build-arg CODEX_CLIENTS_IMAGE ...` or
`Set --build-arg CLIENTS_IMAGE ...`.

For a shared or Kubernetes deployment, build through your registry's approved
architecture-specific or multi-architecture process instead, and build the
native image with an immutable `CLIENTS_IMAGE=repository@sha256:...` reference.
Step 2 of
[Install on Kubernetes and run your first Pod](install-kubernetes.md#2-build-and-publish-the-images)
shows registry publication and digest selection. Then pull the approved native
image onto the local engine and resolve its content ID. Replace the whole
reference in angle brackets with your own:

```sh
SPROOZI_NATIVE_IMAGE='<registry>/sproozi-native@sha256:<approved-digest>'
docker pull "$SPROOZI_NATIVE_IMAGE"
SPROOZI_NATIVE_IMAGE_ID=$(docker image inspect --format '{{.Id}}' "$SPROOZI_NATIVE_IMAGE")
echo "$SPROOZI_NATIVE_IMAGE_ID"
```

Either way, create the bridge network. `SPROOZI_NETWORK` names it, and the
configuration step below writes the same name to `local.networkName`. A name
is 1 to 128 letters, digits, `_`, `.` and `-`, starting with a letter or digit,
and can't be `host` or `none`. Give each home that shares a Docker engine its
own network, as with the image prefix:

```sh
SPROOZI_NETWORK=sproozi-sessions
docker network create "$SPROOZI_NETWORK"
```

The local configuration puts the `sha256:...` content ID in all three image
fields: `nativeImageID`, `helperImageID` and `anchorImageID`. The local adapter
does not pull images or create its bridge network. The remote runtime uses the
canonical registry digest and must make the image available to cluster nodes
separately, so a session that moves to Kubernetes needs the registry path.

The home gateway's fixed IPv4 and port must be reachable from the chosen Docker
network. The trusted network anchor permits sandbox traffic only to that fixed
gateway endpoint, with IPv4/IPv6 restrictions. The anchor has NET_ADMIN for its
fixed firewall; the harness container has no capabilities or host/Docker socket
mounts. Choose cluster Pod and CNI address pools that do not overlap that host
address or your Docker networks. For example, Calico's default `192.168.0.0/16`
contains Docker Desktop's usual host address, `192.168.65.254`. If you use that
host address, choose a different Pod subnet and CNI address pool.

### Configure the local home

These steps configure a local Docker Desktop or Linux engine. Run them in the
same shell as the build, so `SPROOZI_NATIVE_IMAGE_ID` and `SPROOZI_NETWORK`
are still set. They need `openssl` and `python3`. First create the starter
configuration and a private directory for the gateway's files, outside any
repository. If a configuration already exists, `sproozi init` leaves it
unchanged and prints `Kept existing configuration PATH` and the same next steps
again; that is harmless:

```sh
sproozi init
umask 077
SPROOZI_ADMIN="$HOME/.sproozi-admin"
mkdir -p "$SPROOZI_ADMIN/docker-cli"
chmod 700 "$SPROOZI_ADMIN" "$SPROOZI_ADMIN/docker-cli"
case "$(uname -s)" in
  Darwin) SPROOZI_CONFIG="$HOME/Library/Application Support/sproozi/config.json" ;;
  *) SPROOZI_CONFIG="${XDG_CONFIG_HOME:-$HOME/.config}/sproozi/config.json" ;;
esac
SPROOZI_GATEWAY_IP=$(docker run --rm --network "$SPROOZI_NETWORK" --entrypoint getent \
  "$SPROOZI_NATIVE_IMAGE_ID" ahostsv4 host.docker.internal | awk 'NR == 1 {print $1}')
[ -n "$SPROOZI_GATEWAY_IP" ] || SPROOZI_GATEWAY_IP=$(docker network inspect "$SPROOZI_NETWORK" \
  --format '{{(index .IPAM.Config 0).Gateway}}')
echo "$SPROOZI_GATEWAY_IP"
```

`SPROOZI_CONFIG` is the file `sproozi init` printed. `docker-cli` becomes
`dockerConfig`, a private Docker CLI directory for the daemon, not your personal
one. The gateway address is the host as a sandbox sees it: Docker Desktop's
`host.docker.internal`, usually `192.168.65.254`, or on a Linux engine the
network's bridge gateway. On Linux, let the host firewall accept connections
from that bridge to port 18443.

Next, make a private CA and a gateway certificate. The certificate covers the
gateway address as an IP SAN and both model authorities, `api.openai.com` for
Codex and `api.anthropic.com` for Claude Code, as DNS SANs. The CA becomes the
trust bundle that sandboxes receive. Home reads every file its configuration
names, public certificates included, only if it is yours, not a link, and has
no group or other access, such as mode `0600`. The `umask 077` above gives
these files that mode; if you make them in another shell, run
`chmod 600 "$SPROOZI_ADMIN"/*.pem "$SPROOZI_ADMIN"/gateway.* "$SPROOZI_ADMIN"/provider-token`
afterwards:

```sh
cd "$SPROOZI_ADMIN"
openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 30 \
  -addext basicConstraints=critical,CA:TRUE -addext keyUsage=critical,keyCertSign,cRLSign \
  -addext subjectKeyIdentifier=hash \
  -subj '/CN=Sproozi local gateway CA' -keyout ca.key -out ca.pem
printf '%s\n' "subjectAltName=IP:$SPROOZI_GATEWAY_IP,DNS:api.openai.com,DNS:api.anthropic.com" \
  'basicConstraints=critical,CA:FALSE' 'keyUsage=critical,digitalSignature,keyEncipherment' \
  'extendedKeyUsage=serverAuth' 'subjectKeyIdentifier=hash' 'authorityKeyIdentifier=keyid,issuer' \
  > gateway.ext
openssl req -new -newkey rsa:2048 -nodes -subj '/CN=Sproozi local gateway' \
  -keyout gateway.key -out gateway.csr
openssl x509 -req -sha256 -days 30 -in gateway.csr -CA ca.pem -CAkey ca.key \
  -CAcreateserial -extfile gateway.ext -out gateway.crt
openssl verify -x509_strict -purpose sslserver -CAfile ca.pem gateway.crt
printf '%s\n' REPLACE_WITH_PROVIDER_API_KEY > provider-token
cd - >/dev/null
```

The `verify` command prints `gateway.crt: OK`; `serve` checks the SANs itself.
The certificate expires after 30 days; to replace it, move the old files aside
and run this step again.

**`provider-token` now holds only the placeholder `REPLACE_WITH_PROVIDER_API_KEY`.
Write your real provider API key into `$SPROOZI_ADMIN/provider-token` before
you run `sproozi serve`: `serve` refuses to start while the file still holds the
placeholder.** Write the key from your credential manager rather than pasting
it into chat or a workspace. The trusted gateway reads that file; a sandbox
receives only a fresh per-visit proxy token and a placeholder provider key.

Finally, write the remaining settings into the configuration. Set
`SPROOZI_MODEL` to your approved model, `SPROOZI_GATEWAY_PORT` to the home
gateway's port, and `instructions` to your approved text. The script computes `instructionDigest` as the SHA-256 of the exact UTF-8
instruction bytes, which is what `serve` checks:

```sh
SPROOZI_MODEL=gpt-5.5
SPROOZI_GATEWAY_PORT=18443
export SPROOZI_ADMIN SPROOZI_CONFIG SPROOZI_GATEWAY_IP SPROOZI_GATEWAY_PORT SPROOZI_MODEL SPROOZI_NATIVE_IMAGE_ID SPROOZI_NETWORK
export SPROOZI_DOCKER_BINARY="$(command -v docker)"
SPROOZI_DOCKER_SOCKET=$(docker context inspect --format '{{.Endpoints.docker.Host}}')
export SPROOZI_DOCKER_SOCKET="${SPROOZI_DOCKER_SOCKET#unix://}"
python3 -I - <<'EOF'
import hashlib, json, os
e = os.environ
admin, path, image = e["SPROOZI_ADMIN"], e["SPROOZI_CONFIG"], e["SPROOZI_NATIVE_IMAGE_ID"]
port = int(e["SPROOZI_GATEWAY_PORT"])
instructions = "Work only on the repository in this session. Ask before deleting files."
with open(path) as f:
    c = json.load(f)
c.update(dockerBinary=e["SPROOZI_DOCKER_BINARY"], dockerSocket=e["SPROOZI_DOCKER_SOCKET"],
         dockerConfig=admin + "/docker-cli", model=e["SPROOZI_MODEL"], nativeOptions=[])
# Optional: control.sock goes in "directory", and its path must fit 103 bytes on
# macOS. If yours is longer, uncomment and set a short private absolute path;
# serve creates it if missing, and if it exists it must be private (yours,
# mode 700).
# c["directory"] = "/Users/me/.sproozi"
c["local"].update(
    harness="codex", name="local",
    nativeImageID=image, helperImageID=image, anchorImageID=image,
    networkName=e["SPROOZI_NETWORK"], gatewayIPv4=e["SPROOZI_GATEWAY_IP"], gatewayPort=port,
    instructions=instructions,
    instructionDigest=hashlib.sha256(instructions.encode("utf-8")).hexdigest(),
    agentLimits={"MemoryBytes": 1 << 30, "CPUMilli": 1000, "PIDs": 256},
    helperLimits={"MemoryBytes": 512 << 20, "CPUMilli": 500, "PIDs": 128},
    anchorLimits={"MemoryBytes": 64 << 20, "CPUMilli": 100, "PIDs": 32},
    volumeBytes=512 << 20, maxCheckpointBytes=8 << 20, retainedBytes=64 << 20)
c["gateway"] = {
    "listen": f"0.0.0.0:{port}",
    "certificateFile": admin + "/gateway.crt", "keyFile": admin + "/gateway.key",
    "trustBundleFile": admin + "/ca.pem", "providerURL": "https://api.openai.com",
    "providerTokenFile": admin + "/provider-token"}
with open(path + ".new", "w") as f:
    json.dump(c, f, indent=2)
    f.write("\n")
os.replace(path + ".new", path)
print("Updated", path)
EOF
```

The script keeps the rest of the file, including `remote`, `allowances` and
`maxDuration`. It sets both `local.gatewayPort` and the port in
`gateway.listen` to `SPROOZI_GATEWAY_PORT`, 18443 here. Each home on one
machine needs its own port: for a second home, set `SPROOZI_GATEWAY_PORT` to
another port and run the script again, or its `serve` fails with `address
already in use`. For Claude Code, set `harness="claude-code"`, an Anthropic model
and `"providerURL": "https://api.anthropic.com"`, and keep `nativeOptions`
empty. The [home configuration reference](../../cmd/sproozi/README.md#configuration-reference)
and the [configuration schema](../../internal/session/home/config.go) describe
each key.

Once `provider-token` holds your API key, `sproozi serve` passes its
configuration checks, starts the gateway on `SPROOZI_GATEWAY_PORT` and listens on
`control.sock` in the configured `directory`. Once both are up, it prints
`Started home daemon: control socket PATH, gateway HOST:PORT` to standard
error. On a clean shutdown, after Ctrl-C or `SIGTERM`, it prints `Stopped home
daemon`. Keep
`directory` short: a longer socket path than the platform allows (103 bytes on
macOS) fails the configuration check with `control socket path "..." is N
bytes; this platform allows at most 103; set a shorter "directory" in
config.json`. A client command given a `--socket PATH` that is too long ends
instead with `; use a shorter --socket path`.
While `serve` runs, `sproozi sessions` reports the `docker` executor as
`available`. [Start the daemon and conversation](#start-the-daemon-and-conversation)
shows how to keep it running.

After its settings check and before it writes anything, `serve` checks Docker
once: `dockerBinary` must be a file, `dockerSocket` must exist (not checked on
Windows), and the engine must hold `local.nativeImageID` under that exact ID.
If any check fails, `serve` doesn't start. Home then can't answer for any of
its sessions, including `status` and `cancel` for one whose current visit runs
in Kubernetes, until Docker is back and `serve` restarts. A `model` or
`local.instructions` of only spaces counts as missing; values are otherwise
used as written. `dockerSocket` must be an absolute path (on Windows, a
non-blank named pipe), so a value padded with spaces is refused.

#### If `serve` doesn't start

`serve` checks in five passes and stops at the first that fails. Every
message starts with `Sproozi failed: `.

1. **Settings.** It lists every setting that is missing or out of range, one
   per line, by JSON key and the value it wants. This pass reads only the
   configuration and the provider token file:

   ```text
   Sproozi failed: configuration PATH is not ready for serve; set:
     local.gatewayIPv4 (IPv4 address)
     gateway.listen (HOST:PORT on local.gatewayPort)
   To build the images, network and certificates these settings name, see URL
   For each key, see URL
   ```

2. **Docker.** It checks `dockerBinary`, `dockerSocket` and
   `local.nativeImageID`, before anything is written to the home directory.
3. **Gateway port.** It binds `gateway.listen` and releases it at once, still
   before anything is written. If the port is taken, it dials this
   directory's control socket: when a home daemon answers, it reports that
   daemon; otherwise it reports the port in use.
4. **Certificate, listener and saved placement settings.** It opens the home
   directory, checks the gateway certificate and listener, and compares the
   `local` placement settings with the ones saved there.
5. **Listening.** It binds the gateway port and the control socket.

| Pass | Listed setting or message begins | Fix |
| --- | --- | --- |
| 1 | `local.gatewayIPv4 (IPv4 address)` | Set it to a specific IPv4 address that Docker containers reach, such as the network's bridge gateway, not `0.0.0.0` or a multicast address. |
| Before 1 | `Sproozi failed: --config needs a path` | `--config` was given an empty value, for example from an unset variable. Pass the configuration file's path, or omit `--config` for the default. |
| Before 1 | `Sproozi failed: no configuration at PATH; …` | Follow the step it names. For a file not named `config.json` in a directory D, run `sproozi init --directory D` and pass `--config D/config.json`; if D already holds a `config.json`, pass that file or choose another directory. |
| 1 | `local.gatewayPort (port)` | Set the gateway port, such as `18443`. |
| 1 | `local.instructionDigest (SHA-256 of local.instructions as 64 lowercase hex characters, without a sha256: prefix)` | Set it to the SHA-256 of the exact UTF-8 `local.instructions` bytes as 64 lowercase hex characters, as the script above computes. Remove any `sha256:` prefix, and recompute it after editing the instructions. |
| 1 | `gateway.listen (HOST:PORT on local.gatewayPort)` | Use `IP:PORT` or `0.0.0.0:PORT`, where `PORT` equals `local.gatewayPort`. |
| 1 | `gateway.providerTokenFile (replace REPLACE_WITH_PROVIDER_API_KEY in PATH with the provider API key)` | Write your provider API key into that file. |
| 1 | `gateway.maxModelCallSeconds (1 to 3600 seconds; omit it for 1200)` | The value is a whole number outside that range, however large, such as `1e1000000000`. Set the [model call deadline](../../cmd/sproozi/README.md#gateway-certificates-and-provider) to a whole number of seconds from 1 to 3600, or remove the key for 20 minutes. |
| 1 | `Sproozi failed: configuration PATH: gateway.maxModelCallSeconds: X is not a whole number of seconds` | `X` is a fraction, such as `1.5` or `1e-1000000000`, a quoted number or another JSON type. Write a bare whole number from 1 to 3600, such as `1200`; `1200.0` also works. |
| 2 | `Sproozi failed: home: dockerBinary P is not a file` | Set `dockerBinary` to the Docker CLI's absolute path, from `command -v docker`. |
| 2 | `Sproozi failed: home: dockerSocket P does not exist` | Start Docker, or set `dockerSocket` to the engine's Unix socket from `docker context inspect`. |
| 2 | `Sproozi failed: home: dockerBinary B could not inspect local.nativeImageID I through dockerSocket S (...)` | Check that Docker is running, that `dockerBinary` is the Docker CLI and that the native image is loaded. |
| 2 | `Sproozi failed: home: Docker resolved local.nativeImageID I to J` | Set `local.nativeImageID` to the full image ID `J`, not a tag. |
| 2 | `Sproozi failed: home: local.networkName "X" does not exist; create it with "docker network create X"` | Create the bridge network as in [the administrator-owned setup](#prepare-the-administrator-owned-setup), or set `local.networkName` to an existing one. |
| 3 | `Sproozi failed: home: another home daemon is using directory PATH; stop it first ("sproozi serve" holds it until it exits)` | A second `serve` with the same configuration: a home daemon already serves this directory on this port. Use the running daemon, or stop it first. |
| 3 or 5 | `Sproozi failed: home: gateway.listen "ADDR": address already in use; stop the other process or choose another port (and local.gatewayPort)` | Another process, not a home daemon for this directory, holds the port. Stop it, or choose another port and set `local.gatewayPort` to match. Pass 5 reports it only if the port was taken after pass 3. |
| 4 | `Sproozi failed: home: gateway.listen "X" listens on H, which containers cannot reach` | Listen on `local.gatewayIPv4` or `0.0.0.0`. |
| 4 | `Sproozi failed: home: gateway.certificateFile F does not name local.gatewayIPv4 IP` | Reissue the certificate with that address as an IP subject alternative name. |
| 4 | `Sproozi failed: home: gateway.certificateFile F does not name HOST, the inspection host for local.harness K` | Reissue it with that name, `api.openai.com` for Codex or `api.anthropic.com` for Claude Code, as a DNS subject alternative name. |
| 4 | `Sproozi failed: local placement settings changed (local.nativeImageID "…" -> "…"), but N execution records in DIR were prepared under the earlier values; set them back to keep those sessions, or use a new home directory for the new values` | See [changing placement settings](#changing-placement-settings). |
| 4 | `Sproozi failed: home: another home daemon is using directory PATH; stop it first ("sproozi serve" holds it until it exits)` | A second `serve` for the same `directory` with a different gateway port. Use the running daemon, or stop it first. |
| 4 | `Sproozi failed: home: gateway.certificateFile P failed the private-file check; home applies one owner-only check (yours, not a link, no access for any other account) to every file it reads, including public certificates; it must be yours, not a link, with no group or other access: chmod 600 P` | Home reads every configured file, public certificates included, only when no other account can change or replace it. The path in the `chmod` hint is quoted only when it holds characters the shell would treat specially. Run the `chmod` it prints. For a symbolic link, the same message ends instead with `; it must be a real file or directory, not a symbolic link`: replace the link with a copy of the file. The same message names any other configured file, such as `gateway.trustBundleFile`. |
| 4 | `Sproozi failed: home: gateway.providerTokenFile PATH still holds the placeholder REPLACE_WITH_PROVIDER_API_KEY` | The file was reset after the settings pass. Write the provider API key there. |
| 1 or 4 | `gateway.providerTokenFile (PATH is empty; write the provider API key there)`, or in pass 4 `Sproozi failed: home: gateway.providerTokenFile PATH is empty; write the provider API key there` | Write the provider API key into that file. |
| 4 | `Sproozi failed: home: gateway.providerTokenFile PATH has whitespace or NUL inside the key; it must hold only the provider API key` | Rewrite the file with only the key. A trailing newline is fine. |
| 5 | `Sproozi failed: home: control socket PATH: the socket allows access by other accounts (mode 0755); remove it if no home daemon is running, or run chmod 600 on it, then serve again` | A leftover `control.sock` that other accounts can reach. If no home daemon is running, remove it; otherwise run `chmod 600` on it. Then run `serve` again. |
| 5 | `Sproozi failed: home: control socket PATH: the path exists and is not a socket (TYPE); remove it if no home daemon is running, then serve again` | Something other than a socket is at `control.sock`. Remove it if no home daemon is running, then run `serve` again. |
| 5 | `Sproozi failed: home: control socket PATH: another process is listening there; stop it first (a home daemon for directory DIR, or another program using that path)` | Something is already listening on `control.sock`: usually a home daemon for this directory, otherwise another program using that path. A second `serve` for the same directory normally fails earlier, in pass 3 or 4. Stop whichever process is listening, then run `serve` again. |

#### Changing placement settings

The home directory saves the `local` placement settings, such as
`local.nativeImageID`, the network, the gateway address and the resource
limits, the first time `serve` starts. Every execution record is prepared
under those values. You can change them freely while the home has no
execution records: `serve` saves the new values. Once a session has been
prepared, `serve` refuses a change and names each changed setting with its
earlier and new values, the number of execution records and their directory.

Execution records are never deleted, so once a home has prepared any session,
its `local` settings are fixed for that home directory, even after every
session has finished. Upgrading the native image (`local.nativeImageID`) is
such a change. To use new values, point `directory` at a new home directory
and start new sessions there. To keep working on the old sessions, set the
values back, with `directory` pointing at the old home directory.

Allowances need a finite positive unit ceiling and an explicit cost ceiling.
`bounded:false,value:0` means unbounded cost. `bounded:true,value:0` means
exhausted allowance and cannot activate another visit. Bounded model cost needs
administrator pricing in `gateway.pricingFile`.

For a local-only session, leave `remote` empty, as `init` writes it. If a
setting is missing or invalid, `sproozi serve` names it by JSON key. For remote
moves, add a `remote` entry with the remote executor's fixed HTTPS origin,
home principal token file, CA file and checkpoint byte limit.
The remote placement alias must match the profile's `placementName`. All
placements must use the same checkpoint byte limit and approve the same initial
instructions and native version. The original home duration must also fit the
remote executor and live policy; Kubernetes policy currently permits at most one
hour. Moving does not restart the lifetime clock.

## Provision the remote executor

Follow [the profile setup](../../cmd/session-profile/README.md) to install the
current controller/shared gateway and generated CRDs in an evaluation cluster
with a NetworkPolicy-enforcing CNI. The administrator supplies the managed native
runtime, approved template/policy, immutable certificate-only sandbox trust bundle,
shared gateway TLS and gateway-owned provider credentials.

The remote executor uses either its protected host kubeconfig or an in-cluster
ServiceAccount with the documented scoped Run/registration/account/Pod permissions.
It fixes the principal, Run namespace, template, ceilings and maximum duration in
private JSON. Incoming move requests cannot select images, environment variables,
Kubernetes credentials or arbitrary commands. One process owns its private durable
state directory. Run it under a service manager and retain the directory across
restarts.

The private home principal token needs at least 32 non-whitespace characters. TLS 1.3
verifies the profile certificate against the configured CA. The profile client
sends the token only to that exact origin, with no redirect or environment-proxy
fallback. Keep the token and CA file paths in the home configuration; do not copy
the profile's kubeconfig to the developer, sandbox or portable pair.

## Start the daemon and conversation

Keep the daemon process independent of terminal attachments. For a local trial,
run this in the shell where you set `SPROOZI_ADMIN`, which keeps the log with
the administrator files:

```sh
umask 077
nohup sproozi serve </dev/null >"$SPROOZI_ADMIN/daemon.log" 2>&1 &
```

Check that it started: `$SPROOZI_ADMIN/daemon.log` holds the `Started home daemon: control
socket PATH, gateway HOST:PORT` line, and `sproozi sessions` reports the
`docker` executor as `available`. If the log holds configuration errors
instead, the daemon has exited; correct them and start it again. To stop the
daemon, send it `SIGTERM` (`kill PID`). It first drains its model calls for up
to 10 seconds, charging each one in flight as
[interrupted](model-provider-auth.md#gateway-restarts), and the log then ends
with `Stopped home daemon`.

A plain-HTTP request to the gateway port, such as a port check or `curl
http://HOST:PORT`, adds a line like `http: TLS handshake error from ADDR:
client sent an HTTP request to an HTTPS server` to the log. It is harmless:
the gateway accepts only TLS, refused that one connection and keeps running.

The commands in this guide use the configuration that `sproozi init` creates and
its home address. Add `--config FILE` to any of them for another configuration,
or `--socket PATH` for another home address.

Use an ordinary Git repository with a committed HEAD. Stop writers to that source
before capture. The checkpoint retains Git history and branch references,
staged/unstaged changes, deletions, ordinary untracked contents and executable
modes. The source directory remains untouched; all subsequent native work occurs
in the managed Docker workspace.

Start, then keep the session and operation UUIDs printed after it succeeds:

```sh
sproozi start --workspace /absolute/path/to/stopped-repository
```

Ignored files are excluded by default. Start applies the same rules as your Git:
`.gitignore` files, `.git/info/exclude` and the effective `core.excludesFile`
(from the repository config, then your global config, then
`~/.config/git/ignore`). It reads that value without following config includes.
To carry an individual ignored input, use repeatable flags at start:

```sh
sproozi start --workspace /absolute/path/to/stopped-repository \
  --include-ignored needed.bin --include-ignored cache/chosen.dat
```

The ignored-file selection cannot change after start. It remains recorded for
every visit and usage receipt. It accepts at most 32 clean relative file paths
of at most 512 bytes each. It rejects traversal, absolute paths, `.git` control
paths, symlinks, external mounts and special files. Each selected path must
remain an ignored regular file at capture; a missing or unsupported selection
fails the checkpoint. Do not include credential files in the checkpoint.

An initial status can have an empty harness conversation ID (`threadId`). The
harness creates it, and the first successful stopped checkpoint binds that
observed ID once.

## Attach, move and return

Attach to the selected native TUI:

```sh
sproozi attach SESSION_UUID
```

`attach` prints `Attached to the harness terminal; Ctrl-] detaches`. Terminal
resize reaches the harness. Ctrl-] detaches the local client while its daemon
and native process remain alive; attach again with the same command. Ctrl-C while
attached reaches the harness. A reconnect replays a bounded terminal output tail. Avoid
native `/quit` when you intend to move; the placement owns graceful quit and the
independent container stop/checkpoint boundary.

Move to the configured remote executor, attach there, then return to local
Docker. `--to` takes a placement name from the home configuration: `engineering` is
a key of its `remote` map and `local` is its `local.name`, as in the
[configuration example](../../cmd/sproozi/README.md).

```sh
sproozi move SESSION_UUID --to engineering
sproozi attach SESSION_UUID
sproozi move SESSION_UUID --to local
sproozi attach SESSION_UUID
```

A graceful move blocks new terminal input and waits for the current turn and
supported tools to finish within the original deadline. Source gateway access
remains available during that wait. After the turn and tools finish, the
placement revokes access and confirms that the exact agent container has
stopped. It finalizes usage accounting and saves the checkpoint before preparing
another agent. The destination restores state without starting the agent, then
admits the new visit. It cannot reopen a terminal Run or reuse old sandbox
credentials.

The original harness conversation ID (`threadId`), instructions, ignored-file selection and absolute
deadline persist. Remaining allowance decreases by finalized usage, including
outstanding reservations. A move does not refill the allowance or extend the
deadline. Detaching also leaves the writer's deadline and spending rules in
effect.

### Why the commands use different flags

`--executor`, `--from` and `--to` name different things:

- `--executor NAME` picks the [owner](../../GLOSSARY.md#owner) whose catalogue a command addresses.
  `docker` is home's executor name and addresses the home daemon on your
  machine; any other name is a remote executor from the home configuration. An
  explicit `--socket` also selects the home daemon. `home` names the daemon
  itself and `local.name` (`local` in the example) is home's local placement
  for `move --to`; neither is an executor, so `--executor home` and
  `--executor local` fail and point you to `--executor docker`. Home's rows
  in `sessions` show the placement in their `placement` field: the
  `local.name` value or a name under `remote`, empty until the first launch.
- `pull --from NAME` names the remote executor to pull from, configured under
  `remote` or `workSources`: the remote executor that owns the session and
  hands it over to the home daemon. Afterwards `sessions` lists the session
  twice: a `docker` row with `"ownership": "home"`, and a read-only mirror row
  from the remote executor that also says `"ownership": "home"`. See
  [ownership values](../reference/session-commands.md#ownership-values).
- `move --to NAME` names the placement for the next visit. The home daemon
  stays the owner whichever placement runs the execution.

Commands that address a session take its UUID as a positional argument, before or after the flags. `start`
creates the session, so a `start` retry names it with `--id SESSION_UUID`. See
[session commands](../reference/session-commands.md) for the complete routing
and identity contract.

## Recover or cancel

A command refused because an earlier operation is unfinished or cancelled
names the command that unblocks it; see
[recovery messages](../reference/session-commands.md#recovery-messages).
For an uncertain response, first inspect the trusted journal:

```sh
sproozi status SESSION_UUID
sproozi recover SESSION_UUID
```

`start`, `move` and `cancel` print their session and operation UUIDs after the
request succeeds. If the command fails, the error ends with the same IDs to
retry with. Recover continues the retained stage. To
retry explicitly, reuse the same session UUID, operation UUID and exact intent:

```sh
sproozi start --id SESSION_UUID --operation START_OPERATION_UUID \
  --workspace /absolute/path/to/stopped-repository --include-ignored needed.bin
sproozi move SESSION_UUID --operation MOVE_OPERATION_UUID --to engineering
```

The start retry must repeat every original ignored-file flag. A changed operation
or destination cannot supersede an unfinished move. Remote control requests
default to 45 seconds; a slower completed-turn wait can return uncertainty with
input gated. Repeat the exact intent or recover after the backend can establish
its retained state. Attachment disconnects do not cancel the native turn.

If stop is unknown, the execution is unreachable or usage is missing, the visit
stays open and its allowance stays held. The destination cannot start. A lost reply is not proof
of absence or zero consumption. Keep the home and profile journals intact,
restore backend connectivity, and recover the existing operation. Restart a
daemon/profile with the same protected state and configuration; stopping those
processes alone doesn't prove that the agent stopped. The native original
deadline still applies.

Managed remote Pods keep a Sproozi finalizer until the remote executor records
that their harness container stopped. A stopped helper or lost remote executor
can leave a Pod stuck terminating. Restart the same remote executor and run
`recover` for the original operation, and keep its journal and checkpoint files.
Don't remove the finalizer or force-delete the Pod: Sproozi then can't tell
whether the agent stopped, so the allowance stays held and the session can't
move.

Local Docker visits remove their `spz-...` containers and workspace volume
after the stop and finalized usage are saved. A visit whose usage is still
unknown keeps its containers until a retried cancel or recover finalizes it.
Don't remove a visit's containers by hand before then: Sproozi treats a missing
container it did not remove as an unknown stop.

Cancellation forces the execution to stop:

```sh
sproozi cancel SESSION_UUID --operation CANCEL_OPERATION_UUID
```

Cancel persists intent, revokes access first and forces the exact execution to
stop. It keeps the last saved checkpoint and doesn't wait for the current turn
to finish. Until usage is finalized, the allowance stays held. A
cancelled session cannot move again. Retry with the same cancellation UUID or
recover; editing a journal or deleting a registration cannot safely refill it.

## Current limits

The checkpoint must match the selected harness and a real harness conversation ID
(`threadId`).
Codex needs its approved initial instructions and a finished final turn with its
tools. Claude Code needs a compatible primary transcript and the placement's
saved acknowledgement that the turn finished; a final assistant reply alone is
not enough. Selected
native history travels with sanitized Git state;
whole home directories, authentication files, hooks and Git config do not.

Workspace capture currently rejects unborn, conflicted, shallow/partial,
sparse/split-index and linked-worktree repositories, submodules, selected symlinks
and special files. Large archives, unsupported state and bounded storage failures
stop explicitly. Journals and retained artifacts also have limits; there is no
automatic garbage-collection or allowance-reset command.

GUI and IDE attachment, adoption of existing sessions, large or compacted
histories, background tools and subagents are outside the supported workflow.

The reasons for these boundaries are recorded in
[ADR 0008](../adr/0008-selected-native-and-git-checkpoints.md),
[ADR 0009](../adr/0009-runtime-backends-and-sealed-accounting.md) and
[ADR 0010](../adr/0010-managed-session-authority-and-admission.md).
