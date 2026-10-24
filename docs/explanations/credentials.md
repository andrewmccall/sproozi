# How credentials reach an agent

An agent needs a credential to call its model provider. Sproozi offers three
ways to give it one. Every agent offers all three: Codex, Claude Code and
Copilot locally and on Kubernetes, and the other agents on Kubernetes. In
each, the agent's requests go through the [gateway](../../GLOSSARY.md#gateway),
which checks, records and counts every model call.

| Way | Who holds the credential | Where it's stored | Local setup | Kubernetes |
| --- | --- | --- | --- | --- |
| API key held by the gateway | The gateway; the agent never sees it | Your installation's `credentials/` folder, or a gateway Secret | `sproozi init` (the default) | `openai-api-key` or `anthropic-api-key` |
| Sign-in token held by the gateway | The gateway; the agent never sees it | The same | `sproozi init --token` | `openai-sign-in-token`, `anthropic-sign-in-token` or `copilot-sign-in-token` |
| The agent's own sign-in | The agent, which can read it | A [login store](../../GLOSSARY.md#login-store) outside every session | `sproozi init --sign-in`, then sign in inside the first session | `clientConfig.signIn` on the AgentRuntime |

Codex can also use a ChatGPT plan sign-in that the gateway holds and
refreshes (`sproozi init --chatgpt`).

Copilot is the one agent whose provider changes with the way you choose. With
an API key it calls OpenAI in its bring-your-own-key mode. With a token or its
own sign-in it calls GitHub's Copilot service, so it needs a Copilot plan.

## Keys and tokens the gateway holds

The gateway adds the key or token to each request, so a session never
contains it. Each agent has one key or token on a machine or a gateway. This
is the strongest boundary Sproozi offers: an agent that is misled can't send
your credential anywhere.

## The agent's own sign-in

The agent signs in as it would on your machine, for example with `/login`.
Its login file lives in a login store, not in the session:

- On your machine, each login is a Docker volume of your installation.
- On Kubernetes, each login is a PersistentVolumeClaim, which the controller
  creates the first time a session uses it.

Every session of one login shares its store. A sign-in, refresh or sign-out
in one session reaches the others within about a second, as between two
programs on one machine. The supervisor keeps the store and the agent's own
file the same while the agent runs.

A login has a name, `default` unless you choose one, so an agent can use
several accounts, such as work and personal. Set one up with
`sproozi init --harness NAME --sign-in --login work` and start a session on it
with `sproozi run --harness NAME --login work`. A session keeps its login when
it resumes or moves.

Sproozi never copies a login from your machine into a store, and a login never
travels in a checkpoint. A session moved to Kubernetes signs in once there,
with that cluster's store of the same login name.

The agent can read its own sign-in. The gateway refuses a request that carries
it to any other destination it inspects, but it can't see inside an encoded or
split copy.

## Provider terms

Your provider's terms decide which ways you may use. Some providers allow a
subscription only in their own apps, and some forbid sharing a sign-in with
another service. **A sign-in token or a plan sign-in held by the gateway may
breach those terms, so Sproozi doesn't recommend it.** Prefer an API key, or
the agent's own sign-in, when your terms allow it.

Sproozi doesn't check provider terms and has no rules for any provider.
Terms change often, and the choice is yours.

## See also

- [Choose how an agent signs in](../guides/local-sessions.md#choose-how-an-agent-signs-in)
- [Model provider authentication](../guides/model-provider-auth.md) on Kubernetes
- [AgentRuntime sign-in](../reference/agentruntime.md#harness-sign-in)
- [ADR 0047](../adr/0047-harnesses-sign-in-to-their-own-model-providers.md) and
  [ADR 0049](../adr/0049-named-logins-share-a-live-store.md)
