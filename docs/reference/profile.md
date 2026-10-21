# Profile

Your profile is the instructions and skills that every managed Codex and Claude
Code session receives, beside the [Sproozi integration](sproozi-integration.md).
It is one folder you own and edit like your own home directory. Sproozi never
reads your existing `~/.codex` or `~/.claude`.

Like the instructions, a profile is guidance. It grants no access: the gateway
enforces policy whatever the profile says.

## Location and layout

The profile is the `profile` folder beside the configuration file. In a
[local installation](local-installation.md) that is `~/.sproozi/profile`, and
`sproozi init --local` creates its folders. For another configuration, create
the folder beside its `config.json`. A missing folder is an empty profile.

```text
~/.sproozi/
  config.json
  profile/
    AGENTS.md                 optional instructions for every harness
    codex/AGENTS.md           optional instructions for Codex only
    claude/CLAUDE.md          optional instructions for Claude Code only
    skills/
      code-review/
        SKILL.md              required; the skill's name and description
        scripts/check.sh      optional supporting files
```

Write each `SKILL.md` in the format both clients share:

```markdown
---
name: code-review
description: Review a change for correctness and missing tests.
---

Instructions the agent reads when it uses the skill.
```

Other files in the folder are ignored; `sproozi doctor` lists them.

## Rules and limits

| Rule | Value |
| --- | --- |
| Skill folder name | 1–63 lowercase letters, digits or hyphens, starting with a letter or digit |
| Reserved skill name | `sproozi-access` |
| Each instructions file | UTF-8 without NUL bytes, at most 64 KiB |
| Ownership | The folder and every file read must be yours and not writable by group or others; on Windows, private to your account |
| Files | At most 512, regular files only; no symlinks |
| Total size | At most 4 MiB |
| Compressed archive | At most 640 KiB |

An invalid or oversized profile stops the session from launching. In a local
installation, run `sproozi doctor` to check it first; it warns when the archive
passes three quarters of the limit.

## What each harness receives

A harness's instructions are `AGENTS.md`, then its own file, under a
`# User preferences` heading that says they never override the instructions
before them.

| Harness | Instructions | Skills |
| --- | --- | --- |
| Codex | `$CODEX_HOME/AGENTS.md`, after the Sproozi instructions | `$CODEX_HOME/skills/<name>`, beside `sproozi-access` |
| Claude Code | `--append-system-prompt`, after the Sproozi instructions | Plugin `profile`, listed as `profile:<name>` |

Claude Code runs with every settings source disabled, so that hooks, plugins
and personal settings cannot change its managed launch, and in that mode it
reads no user memory file. Its instructions therefore go into the system
prompt.

Repository `AGENTS.md` files follow the profile instructions. Each launch
rebuilds the skills, so a skill you delete is gone at the next launch.
Executable bits on skill files are kept.

## When sessions see changes

Each launch installs the profile as it is then. What the model sees from a
continued conversation differs by harness:

- **A new conversation** receives the current profile instructions and skills.
- **A continued Codex conversation** keeps the instructions from its first
  launch in its history.
- **A continued Claude Code conversation** keeps the system prompt and skill
  listing it started with. The reinstalled profile files are present, but the
  model is not shown the changed instructions or skill descriptions.

**Docker.** The home daemon archives the profile folder when it prepares each
visit and sends it with the launch configuration; nothing from your machine is
mounted into the container. Changes are installed at the session's next
launch: `start`, `resume` or a move back to Docker. The agent cannot change
your folder.

**Kubernetes.** A Kubernetes visit gets the profile as it was when the visit
was launched:

- `move` archives the profile when it creates the destination launch. A
  retried or recovered move sends the same archive.
- `run` sends the profile with the submission. Retrying the same
  `--operation` after editing the profile is rejected as a conflict.

The executor stores each archive once as an immutable ConfigMap named
`sproozi-profile-<digest prefix>` in its run namespace. The controller checks
the archive against its digest, copies it into the run's contract and mounts it
at `/etc/sproozi/profile-archive/profile.tar.gz`. An empty profile sends
nothing.

Stored profiles are not deleted, like session registrations. Each distinct
profile adds one ConfigMap; an administrator can delete ones no retained
session still names.

## Limitations

- A compressed profile over 640 KiB cannot be sent: Docker launches, `run` and
  `move` refuse it.
- Ordinary AgentRuns do not receive the profile.

[ADR 0031](../adr/0031-user-profile-for-managed-sessions.md) records the
design and its tradeoffs.
