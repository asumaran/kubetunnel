# AGENTS.md: operating kubetunnel as an AI agent

Read this before touching kubetunnel on a user's machine. It covers the
day-to-day tasks a user will ask for: installing, adding a tunnel for a
hostname, checking why one is down, updating. For code changes to kubetunnel
itself see [`CLAUDE.md`](./CLAUDE.md).

## What kubetunnel is, in two lines

A root daemon (`kubetunneld`) keeps one `kubectl port-forward` alive per
configured tunnel and fronts them with an HTTPS reverse proxy on
`127.0.0.1:443`. `/etc/hosts` points each configured hostname at `127.0.0.1`
and mkcert issues a locally trusted cert per hostname, so the browser opens the
real URL (`https://service.staging.example.com`) and the request lands on the
pod without ever leaving the machine. `tunnelctl` is the CLI that talks to the
daemon over `/var/run/kubetunnel.sock`.

It is not a VPN and it does not grant access. The user needs working `kubectl`
access to the target cluster and namespace. If `kubectl` cannot port-forward,
kubetunnel cannot either.

## Where things live

| What | Path |
|---|---|
| User config (the only file you edit) | `~/.config/kubetunnel/config.yaml` |
| Certs issued by mkcert | `~/.local/share/kubetunnel/certs/` |
| Logs (JSON lines) | `~/Library/Logs/kubetunnel/{daemon,access,kubectl}.log` |
| LaunchDaemon plist | `/Library/LaunchDaemons/dev.kubetunnel.plist` |
| Control socket | `/var/run/kubetunnel.sock` |
| `/etc/hosts` block | delimited `# kubetunnel` block, rewritten by `sudo tunnelctl install` |

Never commit the user's config into this repo. It holds company hostnames and
cluster names.

## sudo and agents

Three operations need root: installing binaries, rewriting `/etc/hosts`, and
(re)starting the LaunchDaemon. An agent session usually has no TTY for the
password prompt. Do not retry `sudo` in a loop. Hand the user exactly one
command to run themselves and continue when it returns. In Claude Code the `!`
prefix runs it in-session so the output lands in the conversation:

```
! sudo tunnelctl install
```

Everything else (`cert-install`, `reload`, `status`, `logs`, `restart`) runs
as the user and you can run it directly.

## Prerequisites checklist (run these, do not assume)

```bash
command -v go mkcert kubectl                 # brew install go mkcert kubectl
kubectl config get-contexts                  # the target cluster must be here
kubectl --context <ctx> auth can-i create pods/portforward -n <namespace>
kubectl --context <ctx> get svc -n <namespace>   # the target Service exists
```

For GKE the kubeconfig entry comes from
`gcloud container clusters get-credentials <cluster> --region <region> --project <project>`
after `gcloud auth login`. The daemon runs under launchd with a minimal `PATH`,
so the gcloud auth plugin must be reachable through `environment.path_additions`
in the config (`/opt/homebrew/share/google-cloud-sdk/bin` on Apple Silicon
Homebrew, `/usr/local/share/google-cloud-sdk/bin` on Intel). Check with
`ls "$(gcloud info --format='value(installation.sdk_root)')/bin/gke-gcloud-auth-plugin"`.

## First install

```bash
git clone https://github.com/asumaran/kubetunnel.git ~/Developer/kubetunnel
cd ~/Developer/kubetunnel
./scripts/install.sh        # prompts for sudo twice; hand it to the user if you have no TTY
tunnelctl status
```

The script copies `config.example.yaml` to `~/.config/kubetunnel/config.yaml`
when none exists. The example tunnels point at `example.com` hosts and will
sit in `Backoff`; replace them with real tunnels (next section) and reload.

## Recipe: add a tunnel for a hostname

The usual request is "I want to open `https://foo.staging.example.com`
locally". Follow the steps in order.

### 1. Find the Service behind the hostname

`kubectl port-forward` bypasses the cluster's ingress, so you have to find the
backend the ingress would have picked. Look at whatever routes hostnames in
that cluster, in this order:

```bash
CTX=<kube_context>
# Istio VirtualService
kubectl --context $CTX get virtualservice -A -o json \
  | jq -r '.items[] | [.metadata.namespace, .metadata.name, (.spec.hosts|join(","))] | @tsv' \
  | grep <hostname>
# Gateway API HTTPRoute
kubectl --context $CTX get httproute -A -o json \
  | jq -r '.items[] | [.metadata.namespace, .metadata.name, ((.spec.hostnames // [])|join(","))] | @tsv' \
  | grep <hostname>
# Kubernetes Ingress
kubectl --context $CTX get ingress -A -o json \
  | jq -r '.items[] | [.metadata.namespace, .metadata.name, ([.spec.rules[]?.host]|join(","))] | @tsv' \
  | grep <hostname>
```

Then read the matching object (`kubectl get <kind> <name> -n <ns> -o yaml`) and
take the backend Service and port. Watch for:

- A headless "exposer" Service with no selector: follow its endpoints or the
  Deployment it fronts and target the real Service instead.
- One host fanned out to several Services by path (`/api` to one, `/` to
  another): one tunnel per path with `path_prefix`, same `hostname`. The
  longest prefix wins and a tunnel without prefix is the catch-all.
- A URI rewrite in the route (Istio `rewrite.uri`, nginx `rewrite-target`):
  replay it with `strip_prefix`, otherwise the pod receives a path it does not
  serve.

Confirm the Service and its endpoints exist before writing config:

```bash
kubectl --context $CTX get svc,endpointslices -n <ns> -l app=<name>
```

### 2. Pick a free local port

```bash
grep -o 'local_port: [0-9]*' ~/.config/kubetunnel/config.yaml | sort -t: -k2 -n | tail -1
```

Use the next number. Ports are arbitrary; keep them unique.

### 3. Append the tunnel block

```yaml
  - name: <short-unique-name>
    hostname: <hostname>
    kube_context: <kube_context>
    namespace: <ns>
    target: svc/<service>
    remote_port: <service port>
    local_port: <free port>
    # path_prefix: /api        # only when sharing the hostname by path
    # strip_prefix: /api       # only when the ingress rewrites the path
    # health_check:            # only when the target has a cheap health route
    #   path: /healthz
    #   interval: 30s
```

Do not add `health_check` unless you know the endpoint exists. A probe against
a path the app does not serve makes the supervisor restart the tunnel forever.

### 4. Certs first, then reload

```bash
tunnelctl cert-install     # issues the mkcert cert for the new hostname
tunnelctl reload           # hot-reloads config; starts the new tunnel
tunnelctl status           # expect Running / OK
```

Reloading before `cert-install` leaves the proxy without a cert for that SNI
and the browser shows an error page.

### 5. `/etc/hosts` (needs sudo, hand it to the user)

```
! sudo tunnelctl install
```

This rewrites the delimited hosts block from the current config. Until this
runs, the hostname still resolves through public DNS and the request goes to
the real load balancer, so any geo-block or IAP wall in front of it still
applies.

### 6. Verify end to end

```bash
grep <hostname> /etc/hosts                                   # 127.0.0.1 <hostname>
curl -sS -o /dev/null -w 'http=%{http_code} ip=%{remote_ip}\n' https://<hostname>/
```

`ip=127.0.0.1` proves the request went through the tunnel. A public IP means
step 5 did not run or the hostname is misspelled in the config. A `503` with
`Retry-After` means the tunnel is still starting; a `404 no tunnel for <host><path>`
means the path matched no `path_prefix` and there is no catch-all.

## Recipe: remove a tunnel

Delete its block from the config, then `tunnelctl reload` and
`sudo tunnelctl install` (so the hosts entry goes away too).

## Recipe: diagnose a tunnel that is not `Running`

```bash
tunnelctl status
tunnelctl logs --tail 200 --filter 'tunnel:<name>'
tunnelctl debug --name <name>
```

| Symptom in logs | Cause | Fix |
|---|---|---|
| `You must be logged in to the server` | cloud auth expired | `gcloud auth login` (or the provider's equivalent); backoff retries on its own |
| `executable gke-gcloud-auth-plugin not found` | launchd `PATH` lacks the SDK | add the SDK `bin` dir to `environment.path_additions`, `tunnelctl reload` |
| `namespaces "<ns>" not found` / `services "<svc>" not found` | the target is gone (ephemeral PR environments expire) | remove the tunnel |
| `Backoff` with hundreds of restarts | same as above, or no `pods/portforward` permission | `kubectl auth can-i create pods/portforward -n <ns>` |
| `no certificate available (SNI=...)` | cert missing for that hostname | `tunnelctl cert-install`, then `tunnelctl reload` |
| `daemon unreachable: dial unix /var/run/kubetunnel.sock` | daemon not running | `sudo launchctl kickstart -k system/dev.kubetunnel` (user runs it) |
| healthy tunnel, app answers `404`/`403` | missing `strip_prefix` or wrong backend | re-read the route object from step 1 |
| `Running`, but the browser shows the public site's error page | hosts entry missing | `sudo tunnelctl install` |

Remember the environment itself may be down: staging clusters often scale to
zero outside office hours. `kubectl get deploy -n <ns>` with `0/0` replicas is
not a kubetunnel problem.

## Recipe: update kubetunnel

```bash
cd <repo>
git pull
./scripts/update.sh         # rebuilds, installs, restarts the daemon
tunnelctl status
```

`update.sh` does not touch `/etc/hosts`. If the update also added or renamed
hostnames in the config, follow with `sudo tunnelctl install`. Do not use
`tunnelctl install` to apply a code-only update (see `CLAUDE.md` for why).

## Day-2 commands

```bash
tunnelctl status                         # table: state, uptime, restarts, health, route, port, target
tunnelctl dashboard                      # TUI
tunnelctl logs -f                        # live tail
tunnelctl logs --filter 'status:5xx'     # proxy access log
tunnelctl restart --name <name>
tunnelctl reload                         # after any config edit
```

## Things not to do

- Do not run `kubectl port-forward` by hand next to the daemon on the same
  local port; the supervisor owns those ports.
- Do not edit `/etc/hosts` by hand; `tunnelctl install` owns the block.
- Do not add `health_check` paths you have not verified.
- Do not put company hostnames, cluster names or namespaces into this
  repository (README, examples, tests). The repo is public.
