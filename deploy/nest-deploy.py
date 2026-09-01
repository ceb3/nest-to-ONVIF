#!/usr/bin/env python3
"""Automated ONVIF layer deployment for nest-to-ONVIF.

Discovers Nest cameras via the SDM API, updates config.yaml with user choices,
generates ONVIF and MediaMTX configs, installs credentials, configures macvlan
interfaces, and starts Docker Compose.

Requires: Python 3.9+, PyYAML (python3-yaml / pip install pyyaml), Docker with
Compose v2, nest-bridge binary (built automatically when Go is available), and
root for macvlan setup and credential ownership on the deployment host.

Run on the Linux deployment host (or use --remote to push and execute there):

    ./deploy/nest-deploy.py

Non-interactive redeploy with an existing config:

    ./deploy/nest-deploy.py --yes --skip-configure

See deploy/deploy.env.example for persistent host settings.
"""

from __future__ import annotations

import argparse
import ipaddress
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import tempfile
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

try:
    import yaml
except ImportError as exc:  # pragma: no cover
    raise SystemExit(
        "PyYAML is required. Install with: pip install pyyaml  "
        "or: sudo apt install python3-yaml"
    ) from exc

BRIDGE_UID = 10001
BRIDGE_GID = 10001
MAC_PREFIX = "02:4E:53:54:00"
DEFAULT_HOST_IP = "192.168.1.15"
STALE_PATHS = (
    "internal/events/inject.go",
    "internal/events/inject_test.go",
    "internal/protect",
)


class DeployError(Exception):
    """A deployment step failed with a user-facing message."""


@dataclass
class NestDevice:
    name: str
    device_type: str
    protocols: list[str]
    device_id: str

    @property
    def supports_webrtc(self) -> bool:
        return "WEB_RTC" in self.protocols


@dataclass
class CameraChoice:
    device_id: str
    name: str
    audio: bool
    linger: str | None
    mac: str
    ip: str


@dataclass
class DeploySettings:
    repo_root: Path
    config_path: Path
    token_path: Path
    deploy_dir: Path
    install_root: Path
    host_ip: str
    parent_iface: str
    first_camera_ip: str
    bridge_bin: Path
    pubsub_key_src: Path | None
    events_onvif: bool
    non_interactive: bool
    skip_configure: bool
    skip_auth: bool
    skip_systemd: bool
    remote: str | None
    log_level: str


def repo_root_from_script() -> Path:
    return Path(__file__).resolve().parent.parent


def load_deploy_env(deploy_dir: Path) -> dict[str, str]:
    env_path = deploy_dir / "deploy.env"
    if not env_path.is_file():
        return {}
    out: dict[str, str] = {}
    for line in env_path.read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        if "=" not in line:
            continue
        key, value = line.split("=", 1)
        out[key.strip()] = value.strip().strip('"').strip("'")
    return out


def save_deploy_env(deploy_dir: Path, host_ip: str, parent_iface: str, install_root: str) -> None:
    env_path = deploy_dir / "deploy.env"
    content = (
        "# Written by nest-deploy.py — safe to edit between runs.\n"
        f'HOST_IP="{host_ip}"\n'
        f'PARENT_IFACE="{parent_iface}"\n'
        f'INSTALL_ROOT="{install_root}"\n'
    )
    env_path.write_text(content)


def run(cmd: list[str], *, cwd: Path | None = None, check: bool = True, capture: bool = False,
        input_text: str | None = None, env: dict[str, str] | None = None) -> subprocess.CompletedProcess[str]:
    merged = os.environ.copy()
    if env:
        merged.update(env)
    try:
        return subprocess.run(
            cmd,
            cwd=cwd,
            check=check,
            text=True,
            capture_output=capture,
            input=input_text,
            env=merged,
        )
    except subprocess.CalledProcessError as exc:
        detail = exc.stderr.strip() if exc.stderr else exc.stdout.strip() if exc.stdout else str(exc)
        raise DeployError(f"command failed ({' '.join(cmd)}): {detail}") from exc


def require_command(name: str) -> str:
    path = shutil.which(name)
    if not path:
        raise DeployError(f"required command not found: {name}")
    return path


def detect_host_ip() -> str:
    try:
        with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as sock:
            sock.connect(("10.255.255.255", 1))
            return sock.getsockname()[0]
    except OSError:
        return ""


def detect_parent_iface() -> str:
    try:
        result = run(["ip", "route", "show", "default"], capture=True)
    except DeployError:
        return "eth0"
    for line in result.stdout.splitlines():
        parts = line.split()
        if "dev" in parts:
            return parts[parts.index("dev") + 1]
    return "eth0"


def is_placeholder_google(cfg: dict[str, Any]) -> list[str]:
    google = cfg.get("google") or {}
    missing = []
    for key in ("project_id", "client_id", "client_secret"):
        val = str(google.get(key, "")).strip()
        if not val:
            missing.append(key)
            continue
        if "00000000" in val or "xxxxxxxx" in val or val == "secret":
            missing.append(key)
    return missing


def ensure_bridge_binary(repo_root: Path, bridge_bin: Path) -> Path:
    if bridge_bin.is_file():
        return bridge_bin
    candidate = repo_root / "bin" / "nest-bridge"
    if candidate.is_file():
        return candidate
    if shutil.which("go"):
        print("building nest-bridge...")
        run(["make", "build"], cwd=repo_root)
        if candidate.is_file():
            return candidate
    raise DeployError(
        "nest-bridge binary not found. Run 'make build' in the repository root "
        f"or pass --bridge-bin /path/to/nest-bridge"
    )


def load_config(path: Path) -> dict[str, Any]:
    if not path.is_file():
        raise DeployError(f"config not found: {path}")
    with path.open() as fh:
        data = yaml.safe_load(fh) or {}
    if not isinstance(data, dict):
        raise DeployError(f"config root must be a mapping: {path}")
    return data


def backup_file(path: Path) -> None:
    if path.is_file():
        backup = path.with_suffix(path.suffix + ".bak")
        shutil.copy2(path, backup)
        print(f"backed up {path} -> {backup}")


def write_config(path: Path, data: dict[str, Any]) -> None:
    backup_file(path)
    with path.open("w") as fh:
        yaml.safe_dump(data, fh, sort_keys=False, default_flow_style=False)


def list_devices(settings: DeploySettings) -> list[NestDevice]:
    result = run(
        [str(settings.bridge_bin), f"-config={settings.config_path}",
         f"-tokens={settings.token_path}", "devices-json"],
        capture=True,
    )
    try:
        raw = json.loads(result.stdout)
    except json.JSONDecodeError as exc:
        raise DeployError("failed to parse devices-json output") from exc
    devices = [
        NestDevice(
            name=item["name"],
            device_type=item.get("type", ""),
            protocols=item.get("protocols") or [],
            device_id=item["device_id"],
        )
        for item in raw
    ]
    unsupported = [d for d in devices if not d.supports_webrtc]
    for dev in unsupported:
        print(f"warning: {dev.name} does not list WEB_RTC — it may not stream")
    return devices


def existing_camera_map(cfg: dict[str, Any]) -> dict[str, dict[str, Any]]:
    out: dict[str, dict[str, Any]] = {}
    for cam in cfg.get("cameras") or []:
        if isinstance(cam, dict) and cam.get("device_id"):
            out[str(cam["device_id"])] = cam
    return out


def format_mac(index: int) -> str:
    if index < 1 or index > 255:
        raise DeployError(f"camera index {index} out of MAC range 1-255")
    return f"{MAC_PREFIX}:{index:02X}"


def next_ips(start_ip: str, count: int, used: set[str]) -> list[str]:
    base = ipaddress.ip_address(start_ip)
    if not isinstance(base, ipaddress.IPv4Address):
        raise DeployError(f"first camera IP must be IPv4: {start_ip}")
    ips: list[str] = []
    current = int(base)
    while len(ips) < count:
        candidate = str(ipaddress.IPv4Address(current))
        if candidate not in used:
            ips.append(candidate)
        current += 1
        if current > int(ipaddress.IPv4Address("255.255.255.254")):
            raise DeployError("ran out of IPv4 addresses while assigning cameras")
    return ips


def prompt_yes_no(question: str, default: bool) -> bool:
    suffix = "Y/n" if default else "y/N"
    while True:
        answer = input(f"{question} [{suffix}]: ").strip().lower()
        if not answer:
            return default
        if answer in {"y", "yes"}:
            return True
        if answer in {"n", "no"}:
            return False
        print("please answer y or n")


def prompt_string(question: str, default: str) -> str:
    answer = input(f"{question} [{default}]: ").strip()
    return answer or default


def configure_cameras_interactive(
    devices: list[NestDevice],
    cfg: dict[str, Any],
    first_camera_ip: str,
) -> list[CameraChoice]:
    existing = existing_camera_map(cfg)
    used_ips = {
        str((cam.get("onvif") or {}).get("ip"))
        for cam in existing.values()
        if (cam.get("onvif") or {}).get("ip")
    }
    used_macs = {
        str((cam.get("onvif") or {}).get("mac"))
        for cam in existing.values()
        if (cam.get("onvif") or {}).get("mac")
    }

    print("\nNest devices available:")
    for idx, dev in enumerate(devices, start=1):
        proto = ",".join(dev.protocols) or "-"
        marker = "configured" if dev.device_id in existing else "new"
        print(f"  [{idx}] {dev.name} ({dev.device_type}) {proto} — {marker}")

    if existing:
        print("\nCurrently configured:")
        for cam in cfg.get("cameras") or []:
            onvif = cam.get("onvif") or {}
            print(f"  - {cam.get('name')} -> {onvif.get('ip')} ({onvif.get('mac')})")

    default_selection = [str(i) for i, dev in enumerate(devices, start=1) if dev.device_id in existing]
    if not default_selection:
        default_selection = [str(i) for i in range(1, len(devices) + 1)]
    selection = prompt_string(
        "Enter device numbers to deploy (comma-separated), or 'all'",
        ",".join(default_selection) if len(default_selection) != len(devices) else "all",
    )
    if selection.lower() == "all":
        chosen = list(devices)
    else:
        indices: list[int] = []
        for part in selection.split(","):
            part = part.strip()
            if not part:
                continue
            try:
                num = int(part)
            except ValueError as exc:
                raise DeployError(f"invalid device selection: {part}") from exc
            if num < 1 or num > len(devices):
                raise DeployError(f"device number out of range: {num}")
            indices.append(num)
        chosen = [devices[i - 1] for i in indices]

    if not chosen:
        raise DeployError("no cameras selected")

    first_camera_ip = prompt_string("First camera IP (others increment by +1)", first_camera_ip)
    default_audio = prompt_yes_no("Enable audio (AAC transcode) on all selected cameras?", True)
    default_linger = "60s"
    if prompt_yes_no("Use custom motion linger for any camera?", False):
        default_linger = prompt_string("Default linger duration", "60s")

    new_devices = [d for d in chosen if d.device_id not in existing]
    new_ips = next_ips(first_camera_ip, len(new_devices), used_ips)
    new_ip_iter = iter(new_ips)

    mac_index = 1
    while format_mac(mac_index) in used_macs:
        mac_index += 1

    choices: list[CameraChoice] = []
    for dev in chosen:
        prev = existing.get(dev.device_id)
        if prev:
            onvif = prev.get("onvif") or {}
            audio = bool(prev.get("audio", default_audio))
            linger = None
            event = prev.get("event") or {}
            if event.get("linger"):
                linger = str(event["linger"])
            choices.append(
                CameraChoice(
                    device_id=dev.device_id,
                    name=str(prev.get("name") or dev.name),
                    audio=audio,
                    linger=linger,
                    mac=str(onvif.get("mac") or format_mac(mac_index)),
                    ip=str(onvif.get("ip") or next(new_ip_iter)),
                )
            )
            if not onvif.get("mac"):
                mac_index += 1
            continue

        audio = default_audio
        if not prompt_yes_no(f"Enable audio on {dev.name}?", default_audio):
            audio = False
        linger = default_linger if default_linger != "60s" else None
        ip = next(new_ip_iter)
        mac = format_mac(mac_index)
        mac_index += 1
        choices.append(
            CameraChoice(
                device_id=dev.device_id,
                name=dev.name,
                audio=audio,
                linger=linger,
                mac=mac,
                ip=ip,
            )
        )
    return choices


def cameras_to_yaml(choices: list[CameraChoice]) -> list[dict[str, Any]]:
    cameras: list[dict[str, Any]] = []
    for cam in choices:
        entry: dict[str, Any] = {
            "device_id": cam.device_id,
            "name": cam.name,
            "onvif": {"mac": cam.mac, "ip": cam.ip},
        }
        if cam.audio:
            entry["audio"] = True
        if cam.linger and cam.linger != "60s":
            entry["event"] = {"linger": cam.linger}
        cameras.append(entry)
    return cameras


def configure_interactive(settings: DeploySettings, cfg: dict[str, Any], devices: list[NestDevice]) -> dict[str, Any]:
    missing = is_placeholder_google(cfg)
    if missing:
        raise DeployError(
            "config.yaml still has placeholder Google credentials: "
            + ", ".join(missing)
            + ". Fill in google.project_id, client_id, and client_secret first."
        )

    cfg.setdefault("events", {})
    cfg["events"]["onvif"] = prompt_yes_no(
        "Forward Nest detections to ONVIF motion (events.onvif)?", bool(cfg["events"].get("onvif"))
    )
    if cfg["events"]["onvif"]:
        google = cfg.setdefault("google", {})
        google["pubsub_subscription"] = prompt_string(
            "Pub/Sub subscription",
            str(google.get("pubsub_subscription") or "projects/your-gcp-project/subscriptions/sdm-events"),
        )
        google["service_account_key"] = "pubsub-sa.json"
        key_path = prompt_string(
            "Path to Pub/Sub service-account JSON on this machine",
            str(settings.pubsub_key_src or "pubsub-sa.json"),
        )
        settings.pubsub_key_src = Path(key_path).expanduser()

    settings.host_ip = prompt_string("Deployment host IP (docker-compose bindings)", settings.host_ip)
    settings.parent_iface = prompt_string("LAN interface for macvlan parent", settings.parent_iface)
    settings.first_camera_ip = prompt_string("First camera IP for new cameras", settings.first_camera_ip)

    choices = configure_cameras_interactive(devices, cfg, settings.first_camera_ip)
    cfg["cameras"] = cameras_to_yaml(choices)
    cfg.setdefault("media", {})
    cfg["media"].setdefault("rtsp_base_url", "rtsp://127.0.0.1:8554")
    return cfg


def cameras_from_config(cfg: dict[str, Any]) -> list[CameraChoice]:
    choices: list[CameraChoice] = []
    for cam in cfg.get("cameras") or []:
        onvif = cam.get("onvif") or {}
        linger = None
        event = cam.get("event") or {}
        if event.get("linger"):
            linger = str(event["linger"])
        choices.append(
            CameraChoice(
                device_id=str(cam["device_id"]),
                name=str(cam.get("name", "")),
                audio=bool(cam.get("audio")),
                linger=linger,
                mac=str(onvif.get("mac", "")),
                ip=str(onvif.get("ip", "")),
            )
        )
    return choices


def validate_config(cfg: dict[str, Any]) -> None:
    missing = is_placeholder_google(cfg)
    if missing:
        raise DeployError("incomplete Google credentials in config: " + ", ".join(missing))
    choices = cameras_from_config(cfg)
    if not choices:
        raise DeployError("no cameras configured")
    macs: set[str] = set()
    ips: set[str] = set()
    for cam in choices:
        if not cam.device_id or not cam.name:
            raise DeployError("every camera needs device_id and name")
        if not cam.mac or not cam.ip:
            raise DeployError(f"camera {cam.name!r} is missing onvif mac or ip")
        mac = cam.mac.lower()
        if mac in macs:
            raise DeployError(f"duplicate MAC {cam.mac}")
        if cam.ip in ips:
            raise DeployError(f"duplicate IP {cam.ip}")
        macs.add(mac)
        ips.add(cam.ip)
    if cfg.get("events", {}).get("onvif"):
        google = cfg.get("google") or {}
        if not google.get("pubsub_subscription"):
            raise DeployError("events.onvif is true but google.pubsub_subscription is unset")
        if not google.get("service_account_key"):
            raise DeployError("events.onvif is true but google.service_account_key is unset")


def ensure_auth(settings: DeploySettings) -> None:
    if settings.skip_auth and settings.token_path.is_file():
        return
    if settings.token_path.is_file():
        if settings.non_interactive or settings.skip_configure:
            return
        if not prompt_yes_no(f"Re-run OAuth auth (overwrite {settings.token_path})?", False):
            return
    print("starting OAuth flow — complete authorization in your browser...")
    run([str(settings.bridge_bin), f"-config={settings.config_path}",
         f"-tokens={settings.token_path}", "auth"])


def generate_configs(settings: DeploySettings) -> None:
    onvif_path = settings.deploy_dir / "onvif.yml"
    mediamtx_path = settings.deploy_dir / "mediamtx.yml"
    for name, path in (("onvif-config", onvif_path), ("mediamtx-config", mediamtx_path)):
        result = run(
            [str(settings.bridge_bin), f"-config={settings.config_path}", name],
            capture=True,
        )
        path.write_text(result.stdout)
        print(f"wrote {path}")


def patch_compose_host_ip(deploy_dir: Path, host_ip: str) -> None:
    compose_path = deploy_dir / "docker-compose.yml"
    text = compose_path.read_text()
    pattern = re.compile(r'"(\d{1,3}(?:\.\d{1,3}){3}):(8554|8080|8888)')
    matches = {m.group(1) for m in pattern.finditer(text)}
    if not matches:
        raise DeployError(f"could not find host IP bindings in {compose_path}")
    if len(matches) > 1:
        print(f"warning: multiple host IPs in compose ({', '.join(sorted(matches))}); rewriting all")
    for old_ip in matches:
        if old_ip in (host_ip, "127.0.0.1"):
            continue
        text = text.replace(f'"{old_ip}:', f'"{host_ip}:')
    compose_path.write_text(text)
    print(f"docker-compose.yml host bindings -> {host_ip}")


def install_credentials(settings: DeploySettings, cfg: dict[str, Any]) -> None:
    config_dir = settings.deploy_dir / "config"
    config_dir.mkdir(parents=True, exist_ok=True)

    shutil.copy2(settings.config_path, config_dir / "config.yaml")
    shutil.copy2(settings.token_path, config_dir / "tokens.json")

    pubsub_dst = config_dir / "pubsub-sa.json"
    events_on = bool(cfg.get("events", {}).get("onvif"))
    if events_on:
        key_name = str((cfg.get("google") or {}).get("service_account_key") or "pubsub-sa.json")
        src = settings.pubsub_key_src
        if src is None:
            src = settings.repo_root / key_name
        if not src.is_file():
            raise DeployError(
                f"events.onvif is true but Pub/Sub key not found: {src}"
            )
        shutil.copy2(src, pubsub_dst)
    else:
        pubsub_dst.write_text("{}\n")

    for path in (config_dir / "config.yaml", config_dir / "tokens.json"):
        os.chown(path, BRIDGE_UID, BRIDGE_GID)
        os.chmod(path, 0o600)
    os.chown(pubsub_dst, BRIDGE_UID, BRIDGE_GID)
    os.chmod(pubsub_dst, 0o400)
    print(f"installed credentials in {config_dir}")


def install_systemd(settings: DeploySettings) -> None:
    if settings.skip_systemd:
        return
    if os.geteuid() != 0:
        print("skipping systemd install (not root); run with sudo to install units")
        return
    unit_src = settings.deploy_dir / "nest-onvif-macvlan.service"
    dropin_src = settings.deploy_dir / "docker-nest-onvif.conf"
    unit_dst = Path("/etc/systemd/system/nest-onvif-macvlan.service")
    dropin_dir = Path("/etc/systemd/system/docker.service.d")
    dropin_dst = dropin_dir / "nest-onvif.conf"

    text = unit_src.read_text()
    text = re.sub(r"^Environment=PARENT=.*$", f"Environment=PARENT={settings.parent_iface}", text, flags=re.M)
    text = re.sub(
        r"^ExecStart=.*$",
        f"ExecStart={settings.install_root}/deploy/macvlan-setup.sh",
        text,
        flags=re.M,
    )
    unit_dst.write_text(text)
    dropin_dir.mkdir(parents=True, exist_ok=True)
    shutil.copy2(dropin_src, dropin_dst)
    run(["systemctl", "daemon-reload"])
    run(["systemctl", "enable", "--now", "nest-onvif-macvlan.service"])
    print("installed and started nest-onvif-macvlan.service")


def setup_macvlan(settings: DeploySettings) -> None:
    script = settings.deploy_dir / "macvlan-setup.sh"
    env = {"PARENT": settings.parent_iface, "ONVIF_CONFIG": str(settings.deploy_dir / "onvif.yml")}
    if os.geteuid() == 0:
        run(["bash", str(script)], env=env, cwd=settings.deploy_dir)
        return
    if shutil.which("sudo"):
        cmd = ["sudo", "env", f"PARENT={settings.parent_iface}",
               f"ONVIF_CONFIG={settings.deploy_dir / 'onvif.yml'}", "bash", str(script)]
        run(cmd, cwd=settings.deploy_dir)
        return
    raise DeployError("macvlan setup requires root or sudo")


def docker_up(settings: DeploySettings) -> None:
    compose = settings.deploy_dir / "docker-compose.yml"
    env = {"NEST_BRIDGE_LOG_LEVEL": settings.log_level}
    run(["docker", "compose", "-f", str(compose), "build", "bridge", "onvif"], cwd=settings.deploy_dir, env=env)
    run(["docker", "compose", "-f", str(compose), "up", "-d"], cwd=settings.deploy_dir, env=env)
    result = run(["docker", "compose", "-f", str(compose), "ps"], cwd=settings.deploy_dir, capture=True)
    print(result.stdout)


def remove_stale_paths(install_root: Path) -> None:
    for rel in STALE_PATHS:
        path = install_root / rel
        if path.is_file():
            path.unlink()
            print(f"removed stale file {path}")
        elif path.is_dir():
            shutil.rmtree(path)
            print(f"removed stale directory {path}")


def preflight(settings: DeploySettings) -> None:
    if sys.platform != "linux":
        raise DeployError("the ONVIF layer requires Linux (macvlan + host networking)")
    require_command("docker")
    run(["docker", "compose", "version"], capture=True)
    settings.bridge_bin = ensure_bridge_binary(settings.repo_root, settings.bridge_bin)
    if not settings.config_path.is_file():
        example = settings.repo_root / "config.example.yaml"
        if example.is_file() and settings.non_interactive:
            raise DeployError(f"config not found: {settings.config_path}")
        if example.is_file() and prompt_yes_no(f"Create {settings.config_path} from config.example.yaml?", True):
            shutil.copy2(example, settings.config_path)
            print(f"created {settings.config_path} — edit Google credentials, then re-run")
            raise SystemExit(0)


def sync_remote_install(settings: DeploySettings) -> None:
    if not settings.remote:
        return
    remote = settings.remote
    install = settings.install_root
    print(f"syncing repository to {remote}:{install}")
    with tempfile.NamedTemporaryFile(suffix=".tar") as tmp:
        run(["git", "archive", "--format=tar", "-o", tmp.name, "HEAD"], cwd=settings.repo_root)
        run(["ssh", remote, f"sudo mkdir -p {install} && sudo tar -x -C {install}"], input_text=Path(tmp.name).read_bytes().decode("latin-1") if False else None)
        # tar over ssh stdin
        proc = subprocess.Popen(["ssh", remote, f"sudo mkdir -p {install} && sudo tar -x -C {install}"], stdin=subprocess.PIPE)
        assert proc.stdin is not None
        with open(tmp.name, "rb") as fh:
            shutil.copyfileobj(fh, proc.stdin)
        proc.stdin.close()
        if proc.wait() != 0:
            raise DeployError(f"failed to sync repository to {remote}")
    remove_stale_paths(install)
    remote_config = f"{install}/deploy/config"
    run(["ssh", remote, f"sudo mkdir -p {remote_config}"])
    for local, name in ((settings.config_path, "config.yaml"), (settings.token_path, "tokens.json")):
        run(["scp", str(local), f"{remote}:{remote_config}/{name}"])
    if settings.pubsub_key_src and settings.pubsub_key_src.is_file():
        run(["scp", str(settings.pubsub_key_src), f"{remote}:{remote_config}/pubsub-sa.json"])


def parse_args() -> DeploySettings:
    repo_root = repo_root_from_script()
    deploy_dir = repo_root / "deploy"
    env = load_deploy_env(deploy_dir)

    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--config", type=Path, default=repo_root / "config.yaml")
    parser.add_argument("--tokens", type=Path, default=repo_root / "tokens.json")
    parser.add_argument("--bridge-bin", type=Path, default=Path(""))
    parser.add_argument("--install-root", type=Path, default=Path(env.get("INSTALL_ROOT", str(repo_root))))
    parser.add_argument("--host-ip", default=env.get("HOST_IP") or detect_host_ip() or DEFAULT_HOST_IP)
    parser.add_argument("--parent-iface", default=env.get("PARENT_IFACE") or detect_parent_iface())
    parser.add_argument("--first-camera-ip", default="192.168.1.8")
    parser.add_argument("--pubsub-key", type=Path, default=None, help="Pub/Sub service-account JSON (when events.onvif)")
    parser.add_argument("--log-level", default="info", choices=["debug", "info", "warn", "error"])
    parser.add_argument("--remote", help="Deploy to HOST via ssh (e.g. charlie@192.168.1.15)")
    parser.add_argument("--yes", "-y", action="store_true", help="Non-interactive; use existing config")
    parser.add_argument("--skip-configure", action="store_true", help="Do not change config.yaml")
    parser.add_argument("--skip-auth", action="store_true", help="Do not run OAuth")
    parser.add_argument("--skip-systemd", action="store_true", help="Do not install systemd units")
    args = parser.parse_args()

    install_root = args.install_root.resolve()
    deploy_dir = (install_root / "deploy").resolve()
    return DeploySettings(
        repo_root=install_root,
        config_path=args.config.resolve(),
        token_path=args.tokens.resolve(),
        deploy_dir=deploy_dir,
        install_root=install_root,
        host_ip=args.host_ip,
        parent_iface=args.parent_iface,
        first_camera_ip=args.first_camera_ip,
        bridge_bin=args.bridge_bin.resolve() if str(args.bridge_bin) else Path(""),
        pubsub_key_src=args.pubsub_key.resolve() if args.pubsub_key else None,
        events_onvif=False,
        non_interactive=args.yes,
        skip_configure=args.skip_configure,
        skip_auth=args.skip_auth,
        skip_systemd=args.skip_systemd,
        remote=args.remote,
        log_level=args.log_level,
    )


def run_remote(settings: DeploySettings) -> None:
    sync_remote_install(settings)
    remote_cmd = (
        f"cd {settings.install_root} && "
        f"sudo python3 deploy/nest-deploy.py "
        f"--config deploy/config/config.yaml "
        f"--tokens deploy/config/tokens.json "
        f"--install-root {settings.install_root} "
        f"--host-ip {settings.host_ip} "
        f"--parent-iface {settings.parent_iface} "
        f"--skip-auth --yes --skip-configure"
    )
    run(["ssh", settings.remote, remote_cmd])


def main() -> int:
    settings = parse_args()
    try:
        if settings.remote:
            if not settings.non_interactive:
                print("remote deploy uses the local config; run interactively locally first, or pass --yes")
            preflight(settings)
            cfg = load_config(settings.config_path)
            validate_config(cfg)
            ensure_auth(settings)
            if not settings.skip_configure and not settings.non_interactive:
                devices = list_devices(settings)
                cfg = configure_interactive(settings, cfg, devices)
                write_config(settings.config_path, cfg)
            run_remote(settings)
            print("remote deployment finished")
            return 0

        preflight(settings)
        cfg = load_config(settings.config_path)

        if settings.skip_configure or settings.non_interactive:
            validate_config(cfg)
        else:
            ensure_auth(settings)
            devices = list_devices(settings)
            cfg = configure_interactive(settings, cfg, devices)
            write_config(settings.config_path, cfg)
            validate_config(cfg)

        if not settings.skip_auth:
            ensure_auth(settings)

        settings.events_onvif = bool(cfg.get("events", {}).get("onvif"))
        save_deploy_env(settings.deploy_dir, settings.host_ip, settings.parent_iface, str(settings.install_root))
        generate_configs(settings)
        patch_compose_host_ip(settings.deploy_dir, settings.host_ip)
        install_credentials(settings, cfg)
        install_systemd(settings)
        setup_macvlan(settings)
        docker_up(settings)
        print("\ndeployment complete")
        print("adopt each virtual camera in your ONVIF client by IP address")
        return 0
    except DeployError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print("\ncancelled", file=sys.stderr)
        return 130


if __name__ == "__main__":
    raise SystemExit(main())
