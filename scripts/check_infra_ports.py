"""Check Compose ports and subnet without changing other Docker projects."""

import ipaddress
import json
import os
import shutil
import socket
import subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
COMPOSE = ["docker", "compose", "-f", str(ROOT / "scripts/docker-compose.yml")]


def docker_json(arguments):
    result = subprocess.run(
        ["docker", *arguments], cwd=ROOT, check=True, capture_output=True, text=True
    )
    return json.loads(result.stdout)


def check():
    config = docker_json([*COMPOSE[1:], "config", "--format", "json"])
    project = config["name"]
    container_ids = subprocess.run(
        ["docker", "ps", "-aq"], check=True, capture_output=True, text=True
    ).stdout.split()
    containers = docker_json(["inspect", *container_ids]) if container_ids else []
    expected_ports = [
        (service_name, int(port["published"]), int(port["target"]))
        for service_name, service in config["services"].items()
        for port in service.get("ports", [])
    ]
    errors = []
    seen = set()
    for service_name, host_port, target_port in expected_ports:
        if host_port in seen:
            errors.append(f"Duplicate project port: {host_port}")
        seen.add(host_port)
        if 8080 <= host_port <= 8090:
            errors.append(f"Reserved by other backends: {host_port}")
        owner_running = False
        for container in containers:
            labels = container["Config"].get("Labels") or {}
            for bindings in (container["HostConfig"].get("PortBindings") or {}).values():
                for binding in bindings or []:
                    if int(binding["HostPort"]) != host_port:
                        continue
                    if labels.get("com.docker.compose.project") != project:
                        errors.append(
                            f"{host_port} reserved by {container['Name']} "
                            f"({container['State']['Status']})"
                        )
                    elif container["State"]["Running"]:
                        owner_running = True
        if not owner_running:
            try:
                with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe:
                    if os.name == "nt":
                        probe.setsockopt(socket.SOL_SOCKET, socket.SO_EXCLUSIVEADDRUSE, 1)
                    probe.bind(("127.0.0.1", host_port))
            except OSError as error:
                errors.append(f"{host_port} cannot bind: {error}")
        state = "OWN_PROJECT_RUNNING" if owner_running else "BIND_CHECKED"
        print(f"{service_name}: 127.0.0.1:{host_port} -> {target_port} [{state}]")

    wanted = ipaddress.ip_network(config["networks"]["cloudnest"]["ipam"]["config"][0]["subnet"])
    network_ids = subprocess.run(
        ["docker", "network", "ls", "-q"], check=True, capture_output=True, text=True
    ).stdout.split()
    for network in docker_json(["network", "inspect", *network_ids]):
        labels = network.get("Labels") or {}
        if labels.get("com.docker.compose.project") == project:
            continue
        for allocation in (network.get("IPAM") or {}).get("Config") or []:
            subnet = allocation.get("Subnet")
            if subnet:
                existing = ipaddress.ip_network(subnet, strict=False)
                if existing.version == wanted.version and wanted.overlaps(existing):
                    errors.append(f"Subnet overlaps Docker network {network['Name']}: {subnet}")
    if os.name == "nt":
        shell = shutil.which("pwsh") or "powershell"
        route_result = subprocess.run(
            [shell, "-NoProfile", "-Command",
             "Get-NetRoute -AddressFamily IPv4 | "
             "Select-Object -ExpandProperty DestinationPrefix | ConvertTo-Json"],
            check=True, capture_output=True, text=True,
        )
        routes = json.loads(route_result.stdout)
        for route in routes if isinstance(routes, list) else [routes]:
            network = ipaddress.ip_network(route, strict=False)
            if network.prefixlen and wanted.overlaps(network):
                errors.append(f"Subnet overlaps host route: {route}")
    if errors:
        raise SystemExit("\n".join(errors))
    print(f"PASS: ports and subnet {wanted} are isolated from other projects.")


if __name__ == "__main__":
    check()
