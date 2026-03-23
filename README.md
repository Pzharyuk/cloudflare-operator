# Cloudflare Kubernetes Operator

A Kubernetes operator that manages Cloudflare Tunnel ingress rules, DNS records, and Access policies through a single CRD.

## Overview

Define a `TunnelIngress` resource and the operator automatically:

1. **Tunnel** — Adds an ingress rule to your Cloudflare Tunnel
2. **DNS** — Creates/updates a proxied CNAME record pointing to the tunnel
3. **Access** — Manages Cloudflare Access app domains and IP bypass policies
4. **Public IP** — Auto-detects your cluster's public IP and adds it to Access bypass

## Quick Start

```yaml
apiVersion: cloudflare.onit.systems/v1alpha1
kind: TunnelIngress
metadata:
  name: my-app
  namespace: default
spec:
  hostname: app.example.com
  service: http://my-app.default.svc.cluster.local:8080
  access:
    enabled: true
    bypassPublicIP: true
    additionalBypassIPs:
      - 203.0.113.10
  dns:
    proxied: true
```

## How It Works

```
┌─────────────────────────────────────────┐
│          Kubernetes Cluster              │
│                                          │
│  TunnelIngress CRDs    Operator Pod      │
│  ┌──────────────┐     ┌──────────────┐  │
│  │ argocd       │────▶│              │  │
│  │ vault        │     │  Reconciler  │──┼──▶ Cloudflare API
│  │ translate    │────▶│  (30s loop)  │  │    ├─ Tunnel config
│  │ my-new-app   │     │              │  │    ├─ DNS records
│  └──────────────┘     └──────────────┘  │    └─ Access policies
│                              │          │
│                              ▼          │
│                     Auto-detect         │
│                     public IP           │
└─────────────────────────────────────────┘
```

Every 30 seconds the operator:
1. Lists all `TunnelIngress` resources in the cluster
2. Builds the complete tunnel ingress config and PUTs it to Cloudflare
3. Ensures each hostname has a CNAME DNS record pointing to the tunnel
4. Collects all Access-enabled hostnames and updates the Access app
5. Detects the cluster's public IP and updates the Access bypass policy

## Configuration

| Env Variable | Required | Description |
|---|---|---|
| `CF_API_TOKEN` | Yes | Cloudflare API token with DNS, Tunnel, and Access permissions |
| `CF_ACCOUNT_ID` | Yes | Cloudflare account ID |
| `CF_TUNNEL_ID` | Yes | Cloudflare Tunnel ID to manage |
| `CF_ZONE_ID` | Yes | DNS zone ID for the domain |
| `CF_ACCESS_APP_ID` | No | Access application ID (enables Access management) |
| `RECONCILE_INTERVAL` | No | Seconds between reconciliation loops (default: 30) |

## CRD Reference

### TunnelIngress

| Field | Type | Required | Description |
|---|---|---|---|
| `spec.hostname` | string | Yes | Public domain name |
| `spec.service` | string | Yes | Backend service URL |
| `spec.access.enabled` | bool | No | Enable Cloudflare Access protection |
| `spec.access.bypassPublicIP` | bool | No | Auto-add cluster public IP to bypass |
| `spec.access.additionalBypassIPs` | []string | No | Extra IPs for bypass policy |
| `spec.dns.proxied` | bool | No | Proxy through Cloudflare CDN (default: true) |

### Status

```bash
$ kubectl get tunnelingresses
NAME        HOSTNAME                  SERVICE                                              ACCESS   READY   PUBLIC IP
argocd      argocd.onit.systems       http://argocd-server.argocd.svc.cluster.local:80     true     true    50.145.227.126
translate   translate.onit.systems    http://frontend.live-translator.svc.cluster.local:80  false    true    50.145.227.126
vault       vault.onit.systems        http://10.43.80.30:8200                               true     true    50.145.227.126
```

## Deployment

Deployed via ArgoCD as part of the gitops app-of-apps pattern.

## License

MIT
