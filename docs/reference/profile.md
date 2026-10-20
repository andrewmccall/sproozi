# Profile

Your profile is the instructions and skills that every managed Codex and Claude
Code session receives, beside the [Sproozi integration](sproozi-integration.md).
It is one folder you own and edit like your own home directory. Sproozi never
reads your existing `~/.codex` or `~/.claude`.

Like the instructions, a profile is guidance. It grants no access: the gateway
enforces policy whatever the profile says.

## Location and layout

The profile is the `profile` folder beside the configuration file, so with the
defaults it is `~/.sproozi/profile`. `sproozi init` creates it.

```text
~/.sproozi/
  config.json
  profile/
    AGENTS.md                 optional instructions for every session
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

Other files in the folder are ignored.

## Rules and limits

| Rule | Value |
| --- | --- |
| Skill folder name | 1–63 lowercase letters, digits or hyphens, starting with a letter or digit |
| Reserved skill name | `sproozi-access` |
| `AGENTS.md` | UTF-8, at most 64 KiB |
| Files | At most 512, regular files only; no symlinks |
| Total size | At most 4 MiB |
| Compressed archive for Kubernetes | At most 640 KiB |

An invalid profile stops the session from launching. Run `sproozi doctor` to
check it first; it warns when the archive passes three quarters of the
Kubernetes limit.

## What each harness receives

| Harness | Instructions | Skills |
| --- | --- | --- |
| Codex | `$CODEX_HOME/AGENTS.md`, after the Sproozi instructions | `$CODEX_HOME/skills/<name>`, beside `sproozi-access` |
| Claude Code | `--append-system-prompt`, after the Sproozi instructions | Plugin `profile`, listed as `profile:<name>` |

Repository `AGENTS.md` files follow the profile instructions. Each launch
rebuilds the skills, so a skill you delete is gone at the next launch.
Executable bits on skill files are kept.

## When sessions see changes

**Docker.** The agent container mounts the profile folder read-only at
`/etc/sproozi/profile`. The agent cannot change it. Changes reach a session at
its next launch: `start`, `resume` or a move back to Docker.

## Limitations

- On Linux hosts, the agent container runs as UID 1000. Profile files and
  folders must be readable by that UID; `sproozi doctor` warns when they are
  not.
- The Docker engine must be able to bind-mount the profile folder. Docker
  Desktop shares your home directory by default.
- Ordinary AgentRuns do not receive the profile.
