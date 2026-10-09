---
title: "Scale report: what an agent costs"
linkTitle: "Scale report"
weight: 4
aliases:
  - /docs/scale-report/
---

{{% alert title="Preview" color="warning" %}}
These measurements are of the sandbox datapath described in
[Sandboxed agents](../sandboxed-agents/). They were recorded on 2026-08-21
at commit `e5b2966`, when the boundary spoke SOCKS5. The boundary has since
moved to HTTP `CONNECT` and has not been measured again. The shape of the
results is what matters. Single figures are indicative.
{{% /alert %}}

The design gives every agent its own boundary, which resolves names and
enforces policy on each flow. That sounds expensive. This page measures it.
The method is written down so that the result can be challenged, and a
script is provided so that it can be run again. There are two experiments: a
sweep of boundaries attached to one node, and a thousand agents in microVMs
on one host.

## Experiment 1: the boundary

### Questions

1. **Density**: what does the *N*th boundary cost in memory?
2. **Startup**: how long does it take from launching a sandbox until it can
   call the mesh, and does that time grow with the population?
3. **Overhead**: how much latency does an agent pay for reaching the mesh
   through a policy instead of a socket?
4. **Enforcement**: does policy hold under load, and what does a refusal
   cost?

### Method

One real mesh (control plane, router, node) on a kind cluster, as in the
end-to-end suite. Boundaries were attached to the node one at a time, as
host processes and not as containers, because the question is what
`agentmesh-box` costs. A container would measure the container runtime.

The load generator, `agentmesh-bench`, issues the same request every time
(`GET /v1/models`) and enters the mesh in the same way an agent does,
through the boundary's socket. Each step of the sweep was measured in four
conditions:

| Condition | Isolates |
|---|---|
| baseline | the node answering over its own socket, with no boundary in the path |
| reused flow | what an agent normally experiences: one admitted flow, many requests |
| new flow per request | admission: the handshake and the policy decision paid every time |
| denied | enforcement, which leaves no latency behind and must be counted separately |

Client latency shows what an agent experienced. The counters on the servers
show what they did, including the flows they refused. Percentiles are
nearest-rank over the full sample, so every figure was observed. Failed
requests never enter the latency distribution. Warmup requests (20 per
condition) are reported separately, because a first call that pays for
discovery is a real cost.

Environment: Intel Xeon 2.60 GHz, 48 vCPU, 118 GiB, Linux 7.1.6, Go 1.26.5.
The sweep covered 1, 2, 4, 8, 16, 32 and 64 sandboxes, with 500 requests per
condition per step at concurrency 4.

### Results

Latency in milliseconds, memory in MB.

| sandboxes | reused p50 | reused p99 | new-flow p50 | new-flow p99 | admission (µs) | idle RSS | total RSS | denied | leaked |
|---|---|---|---|---|---|---|---|---|---|
| 1 | 1.36 | 2.30 | 1.55 | 78.15 | 19 | 20.2 | 0.02 GiB | 500 | 0 |
| 2 | 1.34 | 2.09 | 1.40 | 3.19 | 20 | 19.4 | 0.06 GiB | 500 | 0 |
| 4 | 1.33 | 2.92 | 1.44 | 3.90 | 19 | 19.0 | 0.12 GiB | 500 | 0 |
| 8 | 1.33 | 2.37 | 1.46 | 6.23 | 18 | 18.8 | 0.21 GiB | 500 | 0 |
| 16 | 1.39 | 3.37 | 1.44 | 2.38 | 19 | 18.9 | 0.39 GiB | 500 | 0 |
| 32 | 1.34 | 2.42 | 1.37 | 3.22 | 18 | 18.1 | 0.72 GiB | 500 | 0 |
| 64 | 1.30 | 1.80 | 1.41 | 4.62 | 18 | 17.4 | 1.37 GiB | 500 | 0 |
| *baseline* | *1.39* | *2.37* | | | | | | | |

Every allowed step served 500 of 500 requests. Every denied step served 0 of
500. "Admission" is the boundary's own mean time to classify a destination
and open it. "Leaked" counts requests that reached a destination that policy
forbade.

**Density.** A boundary settles at 17 to 20 MB resident once idle.
Sixty-four of them, with one under continuous load, came to 1.37 GiB. The
per-sandbox mean falls as the population grows because the same boundary
carries all the load at every step while the others sit idle. The marginal
cost of one more agent is the idle figure, not the mean.

**Startup.** Across 64 sandboxes: median 59 ms, min 58, p95 59, max 70,
where ready means that the socket accepts connections. The 64th sandbox
started as fast as the first.

**Overhead.** Baseline p50 was 1.39 ms. Through the boundary on a reused
flow, p50 ranged from 1.30 to 1.39 ms, at or below baseline at every
population size. The boundary did not make requests faster. Its end-to-end
overhead is smaller than the run-to-run variation of the measurement, so
this method cannot measure it. A fresh flow per request, which pays the
handshake and the policy decision each time, can be measured and is small:
p50 +9 to +157 µs over baseline. The boundary's own in-process
instrumentation put classifying and opening a destination at a mean of 18 to
20 µs, and this moved by less than 2 µs between one sandbox and sixty-four.

**Enforcement.** 3,500 attempts at forbidden destinations, zero successes, at
every population size. The boundary's counter agreed with the client's at
every step. Refusals ran at 24,000 to 28,000 per second, about ten times the
allowed rate, because a denied flow is decided from the name and never
dialled.

### Threats to validity

- One machine, one run, no confidence intervals. The overhead result in
  particular is a case where a single figure could be over-interpreted.
- A trivial request. `/v1/models` is a small response from a warm node. It
  isolates mesh overhead because it does almost no work, and for the same
  reason it says nothing about large payloads or streaming.
- Load through one boundary while the rest sit idle. This answers "do the
  others get in the way" and not "what happens when all *N* are busy".
- A reproducible tail anomaly. The single-sandbox, new-flow step showed p99
  excursions of 60 to 80 ms in every run, and this did not happen at higher
  populations. It is left in the record without an explanation.
- Host processes and not sandboxes. This measures the boundary and not the
  isolation around it. A microVM per agent costs much more, and none of that
  is counted here.

## Experiment 2: a thousand agents on one host

One `n2-standard-64`, one `agentmesh-node` enrolled in the public `bananas`
testnet, and a thousand agents. Each agent is a Firecracker microVM with no
network device, and it reaches the mesh through its own `agentmesh-box` over
vsock. The agent is the example harness: connect, discover tools, attempt a
model call, stop. Raw samples are in
[`tests/scale/results/fleet-1k`](https://github.com/google/agentmesh/tree/main/tests/scale/results/fleet-1k).

### Cost

| | total | per agent |
|---|---|---|
| guest microVMs | 162.7 GiB | **167 MiB** |
| boundaries (`agentmesh-box`) | 24.6 GiB | **25 MiB** |
| node (one, for all) | 386 MiB | 0.4 MiB |
| **together** | **187 GiB of 251 GiB** | **192 MiB** |

The node figure is the one that matters for the design. One `agentmesh-node`
served a thousand distinct agent principals in 386 MB, because an agent is
not a peer. It has no enrollment, no key and no place in the DHT. The mesh
gained a thousand principals and no members.

### Startup

A thousand sandboxes launched in 149 seconds, 6.7 per second, with a median
of 153 ms each and a p95 of 245 ms. The population became reachable as fast
as it was started:

| elapsed | agents serving | microVMs | guest memory |
|---|---|---|---|
| 0 s | 71 | 120 | 17.2 GiB |
| 44 s | 328 | 383 | 62.1 GiB |
| 99 s | 691 | 734 | 119.7 GiB |
| 160 s | **1000** | 1000 | 162.7 GiB |
| 323 s | 1000 | 1000 | 162.7 GiB |

Agents trail microVMs by a few seconds and never by more than about fifty.
That gap is a guest booting and resolving its first mesh name, and not a
queue forming. `agentmesh_node_agents_untracked_total` was 0, so the thousand is a
count and not a limit.

### Throughput

With the thousand agents resident, load was driven through 200 of them at
once, each through its own boundary, so the node saw 200 principals and not
200 connections from one client. Each agent sent 200 requests, with two in
flight at a time.

40,000 requests, none failed, in 0.43 s. That is an aggregate of roughly
92,000 requests per second while the other 800 sandboxes sat resident. Per
agent, the median was 534 req/s (min 490, max 652), so no principal was
starved. Client time to first byte: median 2.0 ms, p95 13 ms, p99 21 ms,
worst 98 ms.

This is an upper bound on what the enforcement path permits. It is not a
throughput claim for a deployed workload. The request was answered by the
node itself, so no upstream provider was in the path. The generator entered
at the boundary's socket and not from inside a guest, so the in-guest stack
and the vsock hop are not counted.

The node's own histogram of the 41,000 server-side durations (including
warmup):

| server-side duration | requests | share |
|---|---|---|
| under 0.5 ms | 35,580 | 86.8% |
| under 5 ms | 39,412 | 96.1% |
| under 10 ms | 40,207 | 98.1% |
| under 25 ms | 40,586 | 99.0% |
| 5 to 10 s | 400 | 1.0% |

The last row is not jitter. Those 400 requests took almost exactly 5.04 s
each, which is the node's DHT lookup timeout. `/v1/models` resolves providers
through the DHT. The walk found four peers, but it asks for up to twenty and
has no way to know that four is all there are, so it waited for the full
timeout and then cached the answer for thirty seconds. The cost is one slow
lookup per cache expiry, paid by whoever arrives on a cold cache. Everything
behind the cache is in the sub-millisecond rows above. It is still a real
problem: the two hundred callers who arrived together each paid the lookup
instead of sharing one.

### What an agent gets

Separately, one sandbox was booted and asked what the mesh would give it. The
sandbox was a microVM with no network device, no API key, no model endpoint
and no tool server address. It had only `mesh.alt` to ask.

| | |
|---|---|
| tools | 15, discovered over MCP with their JSON schemas |
| models | `google/gemma-2-2b-it` (a vLLM instance on a TPU elsewhere in the mesh) and `openrouter/auto` (routed outward) |
| credentials in the sandbox | none |
| network devices in the sandbox | none |

Both models answered. A LangChain agent then ran inside the sandbox, took
its model name from the catalog, and produced output from the TPU-hosted
model, using the stock OpenAI and MCP clients without changes. This shows
that the mesh is reachable. It says nothing about how well a 2B model drives
a fifteen-tool loop.

### Threats to validity

- One host, one run.
- The agents are the example harness. A heavier agent needs more guest
  memory, and 167 MiB describes this workload only.
- Memory was measured with the population idle. Throughput came from a
  separate pass over the same resident fleet.
- Load was driven from the host into each boundary's socket, which skips the
  in-guest stack and the vsock hop. The real per-agent limit is lower.
- Only 200 of the thousand agents sent traffic.
- Inference was not exercised in the thousand-agent run. The agents asked for
  a model named `default`, which no catalog contains, and 998 of those calls
  returned `404`. The inference path is the separate demonstration above.
- A thousand is what fit comfortably in 251 GiB with this agent. Nothing here
  found a limit.
- Spot instances were preempted twice during measurement before this run.
  The final run used a standard instance.

## Conclusions

An agent costs about 192 MiB and 150 ms, and nearly all of that is the
microVM. The boundary is 25 MB of it, and the node's share is under half a
megabyte. The number of agents a host can run is decided by how much memory
the agents want.

The mesh does not grow when the agent population grows. A thousand agents
arrived as one member with one enrollment. This is the reason the principal
is separated from the peer.

Enforcement is not a latency problem. It takes 18 to 20 µs to classify and
open a flow, and the end-to-end cost is too small for this method to
separate from noise. Checking every flow does not have to be traded against
performance, which removes the usual reason for such checks being disabled.

Nothing degraded across the ranges tested, and there were no failures where
success was expected and no successes where it was not.

## Reproducing

[Scale experiment](../scale-experiment/) has the step-by-step procedure. In
short:

```bash
# Experiment 1: docker and kind; stands up a real mesh
./tests/scale/run-density.sh --steps 1,2,4,8,16,32,64 --requests 500

# Experiment 2: a host with KVM, a TUN-capable guest kernel, a bootstrap token
./scripts/provision-scale-vm.sh --no-spot --local-binaries gs://your-bucket/agentmesh
# then on the host
SANDBOX_LINGER=2400 /opt/microvm/launch-microvms.sh 1000
/opt/microvm/collect-fleet.sh --node-socket /var/run/agentmesh-node.sock --duration 1500
```

Results land in `tests/scale/results/` with `environment.json`, the raw
observations and a rendered `table.md`. Single measurements by hand:

```bash
# through a boundary, as an agent would
agentmesh-bench run --socket /run/agent.sock --target http://mesh.alt/v1/models \
  --requests 500 --concurrency 4 --scrape http://127.0.0.1:9600/metrics

# the same request with no boundary, for the baseline
agentmesh-bench run --target-unix /run/node.sock --target http://localhost/v1/models \
  --requests 500 --concurrency 4
```
