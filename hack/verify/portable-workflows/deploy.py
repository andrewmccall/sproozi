#!/usr/bin/env python3
"""Render the owned fixture's trusted profile and stock Hermes Kubernetes stack."""
import argparse
import base64
import json
import os
from pathlib import Path
import secrets

import yaml

ROOT = Path(__file__).resolve().parents[3]


def obj(kind, name, namespace, **fields):
    metadata = dict(name=name)
    if namespace:
        metadata["namespace"] = namespace
    return dict(apiVersion="v1", kind=kind, metadata=metadata, **fields)


def render(fixture, native_image, hermes_image, build_sproozi=False):
    profile = json.loads((fixture / "profile-config.json").read_text())
    home = json.loads((fixture / "home-config.json").read_text())
    if Path(home["gateway"]["providerTokenFile"]).read_text().strip() != "tui-placeholder":
        raise ValueError("Only credential-free fixture deployment is supported")
    credentials = {"tls.crt": (fixture / "gateway.crt").read_bytes(), "tls.key": (fixture / "gateway.key").read_bytes(),
                   "home-token": Path(profile["tokenFile"]).read_bytes(), "task-token": Path(profile["workTokenFile"]).read_bytes()}
    profile.update(listen="0.0.0.0:8443", directory="/state/profile",
                   certificateFile="/var/run/sproozi-work/tls.crt", keyFile="/var/run/sproozi-work/tls.key",
                   tokenFile="/var/run/sproozi-work/home-token", workTokenFile="/var/run/sproozi-work/task-token",
                   repositories={"sproozi": {**profile["repositories"]["sproozi"], "snapshot": "/state/source"}})
    if build_sproozi:
        profile["repositories"]["sproozi"]["snapshot"] = "/state/source-sproozi"
    documents = list(yaml.safe_load_all((ROOT / "examples/portable-workflows/kubernetes/owner.yaml").read_text()))
    for d in documents:
        if d["kind"] == "Deployment":
            pod = d["spec"]["template"]["spec"]
            pod["containers"][0]["image"] = native_image
            # Only this credential-free fixture seeds an administrator-owned Git
            # snapshot. The public deployment uses approved URL sources instead.
            pod["initContainers"] = [{"name": "seed", "image": native_image,
                "command": ["/bin/sh", "-ec", "mkdir -p /state/source; if ! test -d /state/source/.git; then git init --quiet --template= /state/source; printf 'Portable workflow fixture\\n' > /state/source/note.txt; git -C /state/source add note.txt; git -C /state/source -c user.name=Fixture -c user.email=fixture@example.invalid commit --quiet -m 'Initial fixture'; fi"],
                "securityContext": dict(allowPrivilegeEscalation=False, readOnlyRootFilesystem=True, capabilities=dict(drop=["ALL"])),
                "resources": dict(requests=dict(cpu="100m", memory="64Mi"), limits=dict(cpu="500m", memory="256Mi")),
                "volumeMounts": [dict(name="state", mountPath="/state"), dict(name="tmp", mountPath="/tmp")]}]
    documents += [obj("Secret", "sproozi-work-credentials", "sproozi-system",
                       data={k: base64.b64encode(v).decode() for k, v in credentials.items()}),
                  obj("ConfigMap", "sproozi-work-profile", "sproozi-system", data={"profile.json": json.dumps(profile)})]
    coordinator = list(yaml.safe_load_all((ROOT / "examples/orchestration/hermes-coordinator.yaml").read_text()))
    work_config = yaml.safe_load((ROOT / "examples/portable-workflows/kubernetes/hermes-work.yaml").read_text())
    for d in coordinator:
        if d["kind"] == "ConfigMap":
            cfg = yaml.safe_load(work_config["data"]["config.yaml"])
            cfg.update(model=dict(default="fixture-model", provider="custom:fixture"),
                       custom_providers=[dict(name="fixture", base_url="https://192.168.65.254:19865/v1",
                                              key_env="OPENAI_API_KEY", api_mode="chat_completions", model="fixture-model")],
                       platform_toolsets={p: ["sproozi-work"] for p in ("api_server", "cron", "cli")},
                       compression=dict(enabled=False), memory=dict(enabled=False), checkpoints=dict(enabled=False))
            cfg["mcp_servers"] = {"sproozi-work": dict(url="https://sproozi-work.sproozi-system.svc:8443/work/mcp",
                enabled=True, strict_redirect_headers=True, headers=dict(Authorization="Bearer ${SPROOZI_TASK_TOKEN}"),
                connect_timeout=15, timeout=40)}
            d["data"]["config.yaml"] = yaml.safe_dump(cfg, sort_keys=False)
        if d["kind"] == "Deployment":
            d["spec"]["template"]["spec"]["containers"][0]["image"] = hermes_image
    documents += [obj("Namespace", "sproozi-coordinators", None), *coordinator,
                  obj("Secret", "hermes-coordinator", "sproozi-coordinators", stringData={
                      "model-key": "sproozi-kind-coordinator-provider-credential", "task-token": credentials["task-token"].decode().strip(),
                      "api-key": secrets.token_hex(32)}),
                  obj("ConfigMap", "hermes-task-trust", "sproozi-coordinators",
                      data={"ca-bundle.pem": (fixture / "ca.pem").read_text()})]
    return documents


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--fixture", type=Path, required=True)
    parser.add_argument("--images", type=Path, required=True)
    parser.add_argument("--hermes-image", required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--build-sproozi", action="store_true", help="Use separately seeded public Sproozi source under /state/source-sproozi")
    args = parser.parse_args()
    os.umask(0o077)
    images = json.loads(args.images.read_text())
    docs = render(args.fixture.resolve(), images["native"]["image"], args.hermes_image, args.build_sproozi)
    args.output.write_text(yaml.safe_dump_all(docs, sort_keys=False))
    args.output.chmod(0o600)
    print("Wrote private Kubernetes fixture manifest")


if __name__ == "__main__":
    main()
