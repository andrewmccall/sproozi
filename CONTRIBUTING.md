# Contributing

Sproozi is a Kubernetes operator. Start with the repository-specific rules in
`AGENTS.md`, then run the narrowest relevant verification target before
opening a change.

Generated CRDs, RBAC, and deepcopy files must be regenerated with the
Kubebuilder commands described in `AGENTS.md`; do not edit generated output by
hand. Security-sensitive changes should include both an allow and a denial
test, including the expected reason and absence of an upstream side effect.

Please do not include credentials, provider responses, patches, or local
agent configuration in commits.

The repository-owned `verify-sproozi` skill under `.agents/skills/verify-sproozi`
is distributed with the Kind getting-started guide. Its feature map and
`hack/verify/quickstart.py` drive the documented paths and retain local proof.
Start with `python3 hack/verify/quickstart.py doctor`, then run `dependencies`
or the configured provider-backed `demo` path. Other local agent skills and all
credential, kubeconfig and verification artefacts remain private.
