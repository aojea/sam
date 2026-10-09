---
title: "Scale experiment: reproducing the thousand-agent run"
linkTitle: "Scale experiment"
weight: 5
aliases:
  - /docs/scale-experiment/
---

{{% alert title="Preview" color="warning" %}}
The scripts under `tests/scale/` and `scripts/` are research tooling for the
[scale report](../scale-report/). They are kept working but are not a
supported product surface.
{{% /alert %}}

The report says what was measured and what it means. This page says how to
run it again. The setup is one host, one `agentmesh-node`, and *N* agents. Each
agent is a Firecracker microVM with no network device, and its only way out
is a vsock to its own `agentmesh-box`. A thousand agents have been run on one
`n2-standard-64`.

| Component | Role |
|---|---|
| host VM | `n2-standard-64` on GCP with nested virtualisation; runs the node and one `agentmesh-box` per agent |
| guest | Firecracker microVM, Alpine, about 160 MiB, no network device |
| `nano-init` | PID 1 in the guest: builds `tun0`, carries its own TCP stack, keeps the mesh name on every flow |
| agent | the [example harness](https://github.com/google/agentmesh/tree/main/development/examples/agent-harness), holding no credentials |

## Prerequisites

- A guest kernel with `CONFIG_TUN=y`. The Firecracker quickstart, 5.10 and
  6.1 CI kernels do not have it. 6.18.41 does. `startup-script.sh` downloads
  a TUN-capable kernel, and fails with a clear error instead of booting a
  guest whose agent can never get a route.
- A bootstrap token at `/etc/agentmesh-bootstrap-token` on the host. The launcher
  refuses to start without one.
- KVM, with `/dev/kvm` writable by the invoking user.

## 1. Build the guest image

Build once, locally, then upload:

```bash
./scripts/build-rootfs.sh gs://my-agentmesh-bucket/scale-test
```

The image holds Alpine, Python, `nano-init` and the example harness as an
ext4 filesystem. It uses the harness and not the chaos agent, because a
scale run needs the same request every time. To build a different agent
into the image, point `AGENT_SRC` at it and give it enough space. An image
that is too small fails during the copy and not at boot:

```bash
ROOTFS_MB=2048 AGENT_SRC=cmd/chaos-agent ./scripts/build-rootfs.sh
```

## 2. Provision the host

```bash
./scripts/provision-scale-vm.sh \
  --prefix agentmesh-minions --count 1 \
  --machine-type n2-standard-64 \
  --local-binaries gs://my-agentmesh-bucket/scale-test \
  --no-spot
```

This builds your local binaries and injects them with a `startup-script.sh`
that downloads everything on boot. Use `--no-spot` for anything you intend
to keep. Spot capacity for this machine type is limited, a reclaimed run
produces no result, and spot instances are terminated by host maintenance
while standard ones are live-migrated. On `ZONE_RESOURCE_POOL_EXHAUSTED`,
try another zone.

## 3. Size the guest

Run once per agent image:

```bash
sudo tests/scale/measure-guest.sh
```

A guest with too little memory fails in ways that look like network faults.
A guest with too much memory wastes the memory that limits the population.
For the harness: 136 MiB fails, 144 MiB runs slowly, 160 MiB runs at full
speed, against a working set of about 206 MiB.

## 4. Launch

```bash
gcloud compute ssh agentmesh-minions-1 --zone us-central1-c

sudo tests/scale/validate-launcher.sh          # check that one works before asking for a thousand
sudo SANDBOX_LINGER=2400 /opt/microvm/launch-microvms.sh 1000
```

The launcher checks Firecracker, KVM, the kernel, the rootfs, the binaries
and the token. It then starts one `agentmesh-box` per agent and one microVM per
`agentmesh-box`, all against one node. The rootfs is shared read-only. A private
copy per agent would be half a terabyte at a thousand agents.

`SANDBOX_LINGER` keeps each sandbox open after its agent finishes. A density
measurement needs the whole population resident at the same time. By
default a sandbox powers off when it is done, so a thousand sandboxes
started in sequence are never a thousand at once. Zero is the right value
for real work.

| Variable | Meaning |
|---|---|
| `VM_MEM_MIB` | guest memory, default 160 |
| `SANDBOX_LINGER` | seconds to keep a sandbox after the agent exits, default 0 |
| `AGENTMESH_MODEL` | model to ask the mesh for; when unset, the first model in the catalog is used |
| `CONTROL_PLANE` | defaults to the `bananas` testnet |
| `AGENT_DOMAIN` | the domain under which agents are named |

These values reach the guest as kernel command-line pairs, which is the only
way to pass anything to PID 1, so they cannot contain spaces. The launcher
rejects values that do.

## 5. Collect

```bash
sudo tests/scale/collect-fleet.sh \
  --node-socket /var/run/agentmesh-node.sock --duration 1500 --out fleet.jsonl
```

The headline number is how many agents the *node* is serving, and not how
many microVMs were started. A guest that booted and never reached its
boundary is a process, not an agent. The script asks the node and samples
over time, because the shape of the curve shows whether the population came
up smoothly. Watch `agentmesh_node_agents_untracked_total`. If it is not zero, the
agent count is a lower bound and not a measurement.

## 6. Drive load

```bash
sudo tests/scale/load-fleet.sh --agents 200 --requests 200 --out /var/log/load
sudo tests/scale/load-fleet.sh --report /var/log/load
```

Load goes through one boundary per agent, so the node sees *N* principals
and the admission path is exercised as in practice. One generator against
one boundary would measure a socket, not a mesh.

## Watching

```bash
sudo tail -f /var/log/startup-script.log        # host setup
sudo tail -f /var/log/fc-vm-1.log               # one guest's console, agent output included
sudo curl -s --unix-socket /var/run/agentmesh-node.sock http://localhost/metrics | grep agentmesh_node_agents_seen
```

## The boundary sweep

The other half of the report needs no cloud. It starts a real mesh on kind
and attaches boundaries to it:

```bash
./tests/scale/run-density.sh --steps 1,2,4,8,16,32,64 --requests 500
```

Results are written to `tests/scale/results/` with `environment.json`, the
raw per-step observations and a rendered `table.md`.

## Chaos testing

`cmd/chaos-agent` is a LangChain agent pointed at whatever tools the mesh
grants it and told to abuse them. It answers the question "does the mesh
survive an autonomous caller", and not "what does anything cost", because
it never issues the same request twice.

```bash
ROOTFS_MB=2048 AGENT_SRC=cmd/chaos-agent ./scripts/build-rootfs.sh

sudo AGENTMESH_MODEL=google/gemma-2-2b-it CHAOS_SLEEP=30 VM_MEM_MIB=512 \
     /opt/microvm/launch-microvms.sh 25
```

It runs until stopped. It sleeps a random interval between rounds so that a
fleet does not arrive at the providers at the same time, and it treats its
own crashes as data. Like the harness, it holds no credentials and takes its
model from the catalog.
