---
title: "GitHub Codespaces"
linkTitle: "Codespaces"
weight: 5
---

A codespace is a container that GitHub runs for you, with a terminal, an
editor and an `https` URL for every port you forward. Started from this
repository, it gives you a control plane of your own with nothing installed
on your machine and no cloud account. `agentmesh-one` runs inside it, your laptop
and your phone enroll over the public URL, and your GitHub account pays with
its free Codespaces quota (120 core-hours a month on a Free plan; a 2-core
machine is enough). The same setup lets you develop Agent Mesh, or a program that
uses one of its SDKs, against a mesh that external clients can reach.

## 1. Open a codespace

The repository has three dev container configurations. Pick one from the
badge, or from **Code → Codespaces → New with options** on GitHub:

| Configuration | Contents | For |
|---|---|---|
| **testnet** (default badge in the README) | The released `agentmesh-one` and `agentmesh-node` binaries, copied from the `stable` images that also run `hub.sam-mesh.dev`. No toolchain. | Trying Agent Mesh, enrolling your devices. |
| **testnet-latest** | The same, from the `latest` images built from `main`, which also run `bananas.sam-mesh.dev`. | Trying what is not released yet. |
| **develop** (`.devcontainer/devcontainer.json`) | Go, Node, Python and Docker. `make build` runs when the codespace is created, so `./bin` holds the binaries of the branch you opened. | Contributing, or developing an SDK program against your own branch. |

[![Open in GitHub Codespaces](https://github.com/codespaces/badge.svg)](https://codespaces.new/google/agentmesh?quickstart=1&devcontainer_path=.devcontainer%2Ftestnet%2Fdevcontainer.json)

The codespace opens with this page in the editor and a terminal at the
repository root.

## 2. Start the mesh

```bash
make testnet
```

The target runs one command, which is the same command you would run
anywhere else (the codespace checks the repository out under
`/workspaces/agentmesh`):

```bash
agentmesh-one --data-dir /workspaces/agentmesh/.agentmesh-one --port 8080 --tunnel codespaces
```

`--tunnel codespaces` tells `agentmesh-one` that GitHub already forwards the port:
it reads the codespace name and the forwarding domain from the environment,
advertises `https://<codespace>-8080.app.github.dev` as the mesh URL, and
starts nothing. After a moment the banner appears:

```text
══════════════════════════════════════════════════════════════════
Agent Mesh standalone mesh is ready!

API URL:      https://octocat-agentmesh-abc123-8080.app.github.dev
Tunnel:       https://octocat-agentmesh-abc123-8080.app.github.dev -> http://0.0.0.0:8080
Web Console:  https://octocat-agentmesh-abc123-8080.app.github.dev/console
Router Peer:  12D3KooWBzUDQCkZhz2rWrYBhpjcCH8VnrRNcwCW6DoF36iADYrY
Admin Token:  mesh_adm_…
Join Token:   mesh_tok_…

To enroll a node:
  agentmesh-node join https://octocat-agentmesh-abc123-8080.app.github.dev --bootstrap-token-path /workspaces/agentmesh/.agentmesh-one/join-token
══════════════════════════════════════════════════════════════════
```

A QR code for the [mobile app](../../preview/mobile/) follows the banner.

Any `agentmesh-one` flag passes through `ARGS`. To let people log in with an
identity provider instead of the join token, for example:

```bash
make testnet ARGS="--issuer https://accounts.google.com --allowed-audiences <client-id>"
```

The [agentmesh-one reference](../../reference/agentmesh-one/) lists every flag. Outside
a codespace, `make testnet` starts a plain local `agentmesh-one` on port 8080.

## 3. Make the port public

Every forwarded port starts **private**: GitHub's proxy lets your own
browser through and answers everyone else with its login page. Open the
**Web Console** URL from the banner in your browser now and it works. A
`agentmesh-node` on your laptop or the app on your phone cannot log in to GitHub,
so `agentmesh-one` tells you in its log, after a few seconds:

```text
WARN  tunnel  GitHub answers for https://octocat-agentmesh-abc123-8080.app.github.dev: port 8080 is private, so only your own browser can open it. To let devices enroll, make it public: PORTS tab -> right-click 8080 -> Port Visibility -> Public (or `gh codespace ports visibility 8080:public -c octocat-agentmesh-abc123`)
```

Do that once, in the **PORTS** tab next to the terminal, or from your own
machine with the `gh` command from the message (the codespace image does
not include `gh`). `agentmesh-one` keeps checking and confirms within a few
seconds:

```text
INFO  tunnel  https://octocat-agentmesh-abc123-8080.app.github.dev answers from the internet; devices can enroll
```

A public port is reachable by anyone who has the URL, with the same exposure
as a `agentmesh-one` on Cloud Run: `/healthz`, `/info` and the console login page
answer without credentials, enrollment needs the join token or a token you
minted, the console and the admin API need the admin token, and every
router connection needs a credential the control plane issued. The first
boot seeds the open development policy and logs a warning; replace it
before you share the URL, as described in
[Your own mesh](../../getting-started/your-own-mesh/#5-before-you-share-it).

If your organization forbids public ports, keep the port private and let
`agentmesh-one` publish itself through a Cloudflare quick tunnel instead:
`make testnet ARGS="--tunnel cloudflare --tunnel-install"`.

## 4. Enroll your devices

On your laptop, install `agentmesh-node` ([quick start](../../getting-started/quickstart/#1-install)),
save the join token from the banner to a file, and join:

```bash
URL=https://octocat-agentmesh-abc123-8080.app.github.dev
echo -n 'mesh_tok_…' > join-token

agentmesh-node join "$URL" --bootstrap-token-path join-token
agentmesh-node run --daemonize
```

The node appears in the console under **Nodes**. From here the
[Your own mesh](../../getting-started/your-own-mesh/#2-put-a-member-on-it)
walkthrough applies unchanged: publish a model or an MCP server from one
device and call it from another. The second device can be the codespace
itself, where `agentmesh-node` is installed too. It reaches `agentmesh-one` over
loopback, which `--allow-loopback` permits, and `--bind-addr=` keeps its
local API on a Unix socket so it does not compete with `agentmesh-one` for port
8080:

```bash
agentmesh-node run --control-plane http://127.0.0.1:8080 \
  --bootstrap-token-path .agentmesh-one/join-token \
  --data-dir ~/node-a --bind-addr= --allow-loopback
```

Scan the QR code under the banner with the mobile app to enroll a phone.

## 5. Develop against it

In the **develop** configuration, `make testnet` runs `./bin/agentmesh-one`, the
binary built from your branch. Edit, `make build`, stop the mesh with
`Ctrl-C` and start it again; the data directory keeps the identity and the
tokens, and enrolled devices reconnect on their own as long as the mesh is
back within about three minutes.

A program written with a [native SDK](../../guides/native-sdks/) on your
laptop, or a page using the browser SDK, points at the same URL and the
same join token. `make test` and `make lint` run in the codespace like they
do locally; the end-to-end suite needs kind and is better run locally or in
CI.

## What persists and what stops

- **The URL.** The codespace name is fixed for the codespace's lifetime, so
  the URL survives stop and start.
- **The mesh state.** `.agentmesh-one` in the checkout holds the database (members,
  policy, bootstrap tokens), the router key and the two tokens. Git ignores
  it, and it survives stops, starts and container rebuilds, so the router
  keeps its peer ID and devices keep their identity: nothing enrolls twice.
- **The idle stop.** A codespace stops after 30 minutes without activity by
  default; you can raise that to four hours in your GitHub settings. While it
  is stopped nothing answers at the URL. A `agentmesh-node` that finds no router
  for about three minutes exits on purpose, so after a longer stop you start
  your nodes again (`agentmesh-node run --daemonize`; they need no new
  enrollment) or run them under a service manager that restarts them.
  Resume the codespace from [github.com/codespaces](https://github.com/codespaces),
  the README badge, or by connecting to it with `gh codespace code`, and run
  `make testnet` again.
- **Port visibility.** A restart makes the port private again. `agentmesh-one`
  says so in its log, and you set it to public once more.
- **Deletion.** A stopped codespace is deleted after 30 days by default. The
  mesh is gone with it, and devices enroll elsewhere.
- **One mesh per codespace.** The router's relay and discovery state live in
  the single `agentmesh-one` process. A second codespace is a second mesh.

When you want the mesh to stay up, take the same command and its flags to
[Cloud Run](../cloud-run/), [SkyPilot](../skypilot/) or
[Kubernetes](../kubernetes/).
