#!/usr/bin/env python3
"""Render the owned fixture's remote executor and stock Hermes Kubernetes stack."""
import argparse
import base64
import json
import os
from pathlib import Path
import importlib.util
import secrets

import yaml

ROOT = Path(__file__).resolve().parents[3]
_spec = importlib.util.spec_from_file_location("mobility_kubernetes", ROOT / "hack/verify/session-mobility/kubernetes.py")
KUBE = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(KUBE)


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
                   "task-token": (fixture / "work-token").read_bytes()}
    if build_sproozi:
        profile["repositories"]["sproozi"]["snapshot"] = "/state/source-sproozi"
    documents = KUBE.executor_resources(profile, native_image, (fixture / "ca.pem").read_text())
    documents.append(obj("Secret", "sproozi-work-credentials", "sproozi-system",
                         data={k: base64.b64encode(v).decode() for k, v in credentials.items()}))
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
