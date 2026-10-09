# Agent Mesh Scale Experiment Scripts

This directory contains the automation scripts to run massive scale experiments of the Agent Mesh network using Google Cloud Platform (GCP) and Firecracker microVMs.

## Architecture Overview

The scale testing environment is designed to minimize cloud costs while simulating highly realistic isolated workloads:
- **GCP Minion VMs**: Large host machines (e.g., `n2-standard-64`) with nested virtualization enabled.
- **Firecracker MicroVMs**: Dozens of lightweight VMs running on each Minion VM, ensuring strong isolation.
- **Chaos Agent (`cmd/chaos-agent`)**: A LangChain-based Python workload running inside the MicroVM that connects transparently to the Mesh via an MCP loop, fuzzing the endpoints.

## Step-by-Step Guide

### 1. Build the MicroVM Image
Run the `build-rootfs.sh` script locally. This uses Docker to create an Alpine Linux `rootfs.ext4` containing your `chaos-agent` and network proxies (tun2proxy/socat).

```bash
# Build locally and automatically upload to your Google Cloud Storage bucket
./scripts/build-rootfs.sh gs://my-agentmesh-bucket/scale-test
```

### 2. Provision the Cloud Minions
Use the `provision-scale-vm.sh` script to bulk-create your GCP Minion VMs. The script automatically compiles your local `agentmesh-node` and `agentmesh-box` binaries, uploads them, and configures the VMs via `cloud-init.yaml`.

```bash
# Provision 100 VMs using your local binaries and rootfs
./scripts/provision-scale-vm.sh --prefix agentmesh-minions --count 100 --local-binaries gs://my-agentmesh-bucket/scale-test
```

### 3. Launch MicroVMs
Once the GCP Minions boot up, their `cloud-init` automatically downloads Firecracker, your rootfs, and the latest Agent Mesh binaries. 
SSH into a Minion VM (or use a startup-script wrapper) and run the launcher:

```bash
# Launch 20 isolated microVMs per host
/opt/microvm/launch-microvms.sh 20
```

Each microVM is spawned with a dedicated `agentmesh-box` running on the host. Network traffic is transparently routed out of the guest using `tun2proxy` over Firecracker VSOCK, hitting the local `agentmesh-box` UDS endpoint.
