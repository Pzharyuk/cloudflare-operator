# Multi-Account Cloudflare Operator Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extend the Cloudflare Kubernetes operator to support multiple Cloudflare accounts, each with their own API token, account ID, tunnel, and zone, with TunnelIngress CRs selecting accounts by name or hostname domain match.

**Architecture:** Add an `AccountRegistry` to the cloudflare package that reads multi-account credentials from env vars (`CF_ACCOUNT_<NAME>_*`), preserving backward compatibility with the legacy single-env-var format. The reconciler drops its single `Config`/`*Client` pair and instead holds the registry, groups incoming TunnelIngress objects by resolved account, and runs per-account reconcile passes.

**Tech Stack:** Go 1.23, k8s.io/client-go (dynamic client), k8s.io/apimachinery, hand-rolled operator (no controller-runtime)

---

## What We're Changing and Why

### Current state
- `cmd/operator/main.go` — reads 4 required env vars (`CF_API_TOKEN`, `CF_ACCOUNT_ID`, `CF_TUNNEL_ID`, `CF_ZONE_ID`) + 1 optional (`CF_ACCESS_APP_ID`), creates one client, one `controller.Config`.
- `internal/cloudflare/client.go` — single `Client` struct, no concept of multiple accounts.
- `internal/controller/reconciler.go` — `Reconciler` holds one `Config` + `*cloudflare.Client`. All TunnelIngress resources are processed through the same tunnel/zone/access app.
- `internal/crd/types.go` — `TunnelIngressSpec` has no account selector.
- `deploy/crd.yaml` — CRD schema has no `account` field.

### After this change
- `internal/cloudflare/accounts.go` (new) — `AccountConfig` struct, `AccountRegistry` with `LoadFromEnv()` and `Resolve()`.
- `internal/cloudflare/client.go` — unchanged (the `Client` type stays as-is).
- `internal/crd/types.go` — `TunnelIngressSpec` gains optional `Account string` field.
- `cmd/operator/main.go` — calls `cloudflare.LoadFromEnv()`, passes registry to controller; `requireEnv` helper removed.
- `internal/controller/reconciler.go` — `Reconciler` holds `*cloudflare.AccountRegistry`; `Config` struct removed; reconcile methods take explicit `*cloudflare.AccountConfig` and `*cloudflare.Client` parameters; new `groupByAccount()` method.
- `deploy/crd.yaml` — `account` field added to OpenAPI schema.

---

## Env Var Convention Reference

| Purpose | Env var | Required |
|---------|---------|----------|
| Multi-account API token | `CF_ACCOUNT_<NAME>_API_TOKEN` | Yes |
| Multi-account account ID | `CF_ACCOUNT_<NAME>_ACCOUNT_ID` | Yes |
| Multi-account tunnel ID | `CF_ACCOUNT_<NAME>_TUNNEL_ID` | Yes |
| Multi-account zone ID | `CF_ACCOUNT_<NAME>_ZONE_ID` | Yes |
| Domain for hostname matching | `CF_ACCOUNT_<NAME>_ZONE_DOMAIN` | No |
| Cloudflare Access app | `CF_ACCOUNT_<NAME>_ACCESS_APP_ID` | No |
| Explicit default fallback | `CF_DEFAULT_ACCOUNT` | No |
| **Legacy** API token | `CF_API_TOKEN` | Legacy only |
| **Legacy** account ID | `CF_ACCOUNT_ID` | Legacy only |
| **Legacy** tunnel ID | `CF_TUNNEL_ID` | Legacy only |
| **Legacy** zone ID | `CF_ZONE_ID` | Legacy only |
| **Legacy** zone domain | `CF_ZONE_DOMAIN` | No |
| **Legacy** Access app | `CF_ACCESS_APP_ID` | No |

`<NAME>` is case-insensitive in the env var; the operator normalizes to lowercase for internal use and CR matching.

---

## Task 1: Add `account` field to CRD types and YAML

**Files:**
- Modify: `internal/crd/types.go:39-48` (TunnelIngressSpec struct)
- Modify: `deploy/crd.yaml:18-39` (spec.properties block)

- [ ] **Step 1: Add `Account` field to `TunnelIngressSpec`**

In `internal/crd/types.go`, update the struct (no `DeepCopyInto` change needed — plain string copies automatically):

```go
type TunnelIngressSpec struct {
	// Hostname is the public domain (e.g., argocd.onit.systems)
	Hostname string `json:"hostname"`
	// Service is the backend URL (e.g., http://argocd-server.argocd.svc.cluster.local:80)
	Service string `json:"service"`
	// Account selects which Cloudflare account to use for this ingress.
	// Must match a name configured via CF_ACCOUNT_<NAME>_* env vars (case-insensitive).
	// If omitted, the operator matches by hostname domain suffix or falls back to the default account.
	Account string `json:"account,omitempty"`
	// Access configures Cloudflare Access protection
	Access *AccessSpec `json:"access,omitempty"`
	// DNS configures the DNS record (defaults to proxied CNAME)
	DNS *DNSSpec `json:"dns,omitempty"`
}
```

- [ ] **Step 2: Add `account` field to `deploy/crd.yaml` OpenAPI schema**

In `deploy/crd.yaml`, add after the `service` property block (inside `spec.properties`):

```yaml
                account:
                  type: string
                  description: >
                    Cloudflare account name (case-insensitive, matches CF_ACCOUNT_<NAME>_* env var prefix).
                    If omitted, the operator matches by hostname domain suffix or uses the default account.
```

The full updated `spec.properties` block will look like:

```yaml
              properties:
                hostname:
                  type: string
                  description: Public domain (e.g., app.example.com)
                service:
                  type: string
                  description: Backend URL (e.g., http://svc.ns.svc.cluster.local:80)
                account:
                  type: string
                  description: >
                    Cloudflare account name (case-insensitive, matches CF_ACCOUNT_<NAME>_* env var prefix).
                    If omitted, the operator matches by hostname domain suffix or uses the default account.
                access:
                  type: object
                  properties:
                    enabled:
                      type: boolean
                    bypassPublicIP:
                      type: boolean
                    additionalBypassIPs:
                      type: array
                      items:
                        type: string
                dns:
                  type: object
                  properties:
                    proxied:
                      type: boolean
```

- [ ] **Step 3: Verify the file compiles (types only)**

```bash
cd /path/to/repo && go build ./internal/crd/...
```

Expected: no output (success).

- [ ] **Step 4: Commit**

```bash
git add internal/crd/types.go deploy/crd.yaml
git commit -m "feat(crd): add optional account field to TunnelIngressSpec"
```

---

## Task 2: Create `internal/cloudflare/accounts.go`

**Files:**
- Create: `internal/cloudflare/accounts.go`

This is the core of the multi-account feature. It provides `LoadFromEnv()` (reads credentials) and `Resolve()` (picks the right account for an ingress).

- [ ] **Step 1: Create the file**

Create `internal/cloudflare/accounts.go` with the following content:

```go
package cloudflare

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// AccountConfig holds the per-account Cloudflare configuration (tunnel, zone, Access).
// Credentials (API token, account ID) live in the paired Client, not here.
type AccountConfig struct {
	Name        string // lowercase account name, matches spec.account in TunnelIngress
	TunnelID    string
	ZoneID      string
	ZoneDomain  string // optional base domain for hostname-suffix matching (e.g. "onit.systems")
	AccessAppID string // optional Cloudflare Access application ID
}

// accountEntry pairs an AccountConfig with its API client.
type accountEntry struct {
	cfg    AccountConfig
	client *Client
}

// AccountRegistry holds all configured Cloudflare accounts and resolves which
// account to use for a given TunnelIngress hostname or explicit account name.
type AccountRegistry struct {
	accounts    map[string]*accountEntry // keyed by lowercase account name
	defaultName string                   // fallback when no explicit or domain match is found
}

// LoadFromEnv reads Cloudflare credentials from environment variables and returns
// a populated AccountRegistry.
//
// Multi-account env var format (NAME is case-insensitive; stored lowercase):
//
//	CF_ACCOUNT_<NAME>_API_TOKEN     — required
//	CF_ACCOUNT_<NAME>_ACCOUNT_ID    — required
//	CF_ACCOUNT_<NAME>_TUNNEL_ID     — required
//	CF_ACCOUNT_<NAME>_ZONE_ID       — required
//	CF_ACCOUNT_<NAME>_ZONE_DOMAIN   — optional, enables hostname-suffix account matching
//	CF_ACCOUNT_<NAME>_ACCESS_APP_ID — optional
//
// Legacy single-account format (backward compatible; treated as account name "default"):
//
//	CF_API_TOKEN, CF_ACCOUNT_ID, CF_TUNNEL_ID, CF_ZONE_ID (all required if any is set)
//	CF_ZONE_DOMAIN (optional), CF_ACCESS_APP_ID (optional)
//
// When multiple accounts are configured without legacy vars, set CF_DEFAULT_ACCOUNT=<name>
// to choose which account is the fallback for ingresses with no account match.
func LoadFromEnv() (*AccountRegistry, error) {
	reg := &AccountRegistry{accounts: make(map[string]*accountEntry)}

	// --- Legacy single-account support ---
	if token := os.Getenv("CF_API_TOKEN"); token != "" {
		accountID := os.Getenv("CF_ACCOUNT_ID")
		tunnelID := os.Getenv("CF_TUNNEL_ID")
		zoneID := os.Getenv("CF_ZONE_ID")
		if accountID == "" || tunnelID == "" || zoneID == "" {
			return nil, fmt.Errorf("legacy account: CF_ACCOUNT_ID, CF_TUNNEL_ID, and CF_ZONE_ID are required when CF_API_TOKEN is set")
		}
		entry := &accountEntry{
			cfg: AccountConfig{
				Name:        "default",
				TunnelID:    tunnelID,
				ZoneID:      zoneID,
				ZoneDomain:  os.Getenv("CF_ZONE_DOMAIN"),
				AccessAppID: os.Getenv("CF_ACCESS_APP_ID"),
			},
			client: NewClient(token, accountID),
		}
		reg.accounts["default"] = entry
		reg.defaultName = "default"
		slog.Info("loaded legacy Cloudflare account", "name", "default")
	}

	// --- Multi-account: scan env for CF_ACCOUNT_<NAME>_API_TOKEN ---
	const envPrefix = "CF_ACCOUNT_"
	const envSuffix = "_API_TOKEN"
	seen := make(map[string]bool)

	for _, env := range os.Environ() {
		kv := strings.SplitN(env, "=", 2)
		key := kv[0]
		if !strings.HasPrefix(key, envPrefix) || !strings.HasSuffix(key, envSuffix) {
			continue
		}
		// Extract uppercase NAME, e.g. "HOME" from "CF_ACCOUNT_HOME_API_TOKEN"
		nameUpper := strings.TrimSuffix(strings.TrimPrefix(key, envPrefix), envSuffix)
		nameLower := strings.ToLower(nameUpper)
		if seen[nameLower] {
			continue // avoid reprocessing if env has duplicates
		}
		seen[nameLower] = true

		p := envPrefix + nameUpper + "_"
		token := os.Getenv(p + "API_TOKEN")
		accountID := os.Getenv(p + "ACCOUNT_ID")
		tunnelID := os.Getenv(p + "TUNNEL_ID")
		zoneID := os.Getenv(p + "ZONE_ID")
		if token == "" || accountID == "" || tunnelID == "" || zoneID == "" {
			return nil, fmt.Errorf("account %q: %sAPI_TOKEN, %sACCOUNT_ID, %sTUNNEL_ID, and %sZONE_ID are all required",
				nameLower, p, p, p, p)
		}
		entry := &accountEntry{
			cfg: AccountConfig{
				Name:        nameLower,
				TunnelID:    tunnelID,
				ZoneID:      zoneID,
				ZoneDomain:  os.Getenv(p + "ZONE_DOMAIN"),
				AccessAppID: os.Getenv(p + "ACCESS_APP_ID"),
			},
			client: NewClient(token, accountID),
		}
		reg.accounts[nameLower] = entry
		slog.Info("loaded Cloudflare account", "name", nameLower, "tunnelID", tunnelID, "zoneID", zoneID)
	}

	if len(reg.accounts) == 0 {
		return nil, fmt.Errorf("no Cloudflare accounts configured; set CF_API_TOKEN or CF_ACCOUNT_<NAME>_API_TOKEN")
	}

	// Determine default account for fallback
	if explicitDefault := strings.ToLower(os.Getenv("CF_DEFAULT_ACCOUNT")); explicitDefault != "" {
		if _, ok := reg.accounts[explicitDefault]; !ok {
			return nil, fmt.Errorf("CF_DEFAULT_ACCOUNT=%q does not match any configured account", explicitDefault)
		}
		reg.defaultName = explicitDefault
	} else if reg.defaultName == "" {
		// No legacy account, no explicit default — if there's only one account, make it default
		if len(reg.accounts) == 1 {
			for name := range reg.accounts {
				reg.defaultName = name
			}
		} else {
			slog.Warn("multiple accounts configured with no default; ingresses with no account match will fail — set CF_DEFAULT_ACCOUNT to fix")
		}
	}

	return reg, nil
}

// Names returns the names of all registered accounts (in no particular order).
func (r *AccountRegistry) Names() []string {
	names := make([]string, 0, len(r.accounts))
	for name := range r.accounts {
		names = append(names, name)
	}
	return names
}

// Get returns the AccountConfig and Client for the named account.
// Returns an error if the name is not registered.
func (r *AccountRegistry) Get(name string) (*AccountConfig, *Client, error) {
	e, ok := r.accounts[name]
	if !ok {
		return nil, nil, fmt.Errorf("account %q not found; configured: %v", name, r.Names())
	}
	return &e.cfg, e.client, nil
}

// Resolve determines which account name to use for a TunnelIngress.
//
// Resolution order:
//  1. If accountName is non-empty (spec.account field set), use it directly.
//  2. Scan accounts with a ZoneDomain configured — if hostname ends with ".<domain>"
//     or equals the domain exactly, use that account.
//  3. Fall back to the default account (set via CF_DEFAULT_ACCOUNT or auto-detected).
//
// Returns an error if no account can be resolved.
func (r *AccountRegistry) Resolve(accountName, hostname string) (string, error) {
	// 1. Explicit account name
	if accountName != "" {
		nameLower := strings.ToLower(accountName)
		if _, ok := r.accounts[nameLower]; !ok {
			return "", fmt.Errorf("spec.account=%q not found; configured accounts: %v", accountName, r.Names())
		}
		return nameLower, nil
	}

	// 2. Hostname domain suffix match
	for name, e := range r.accounts {
		if e.cfg.ZoneDomain == "" {
			continue
		}
		if hostname == e.cfg.ZoneDomain || strings.HasSuffix(hostname, "."+e.cfg.ZoneDomain) {
			return name, nil
		}
	}

	// 3. Default fallback
	if r.defaultName != "" {
		return r.defaultName, nil
	}

	return "", fmt.Errorf("no matching account for hostname %q and no default configured", hostname)
}
```

- [ ] **Step 2: Verify the cloudflare package compiles**

```bash
cd /path/to/repo && go build ./internal/cloudflare/...
```

Expected: no output (success).

- [ ] **Step 3: Commit**

```bash
git add internal/cloudflare/accounts.go
git commit -m "feat(cloudflare): add AccountRegistry with multi-account env var loading"
```

---

## Task 3: Update `cmd/operator/main.go`

**Files:**
- Modify: `cmd/operator/main.go`

Replace the single-credential block with `cloudflare.LoadFromEnv()`. The `controller.New` signature will change in the next task, but we update `main.go` now to match.

- [ ] **Step 1: Rewrite `main.go`**

Replace the entire file content:

```go
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Pzharyuk/cloudflare-operator/internal/cloudflare"
	"github.com/Pzharyuk/cloudflare-operator/internal/controller"

	"k8s.io/client-go/rest"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	// Load all Cloudflare account credentials from environment variables.
	// Supports both the legacy single-account format (CF_API_TOKEN, etc.) and
	// multi-account format (CF_ACCOUNT_<NAME>_API_TOKEN, etc.).
	registry, err := cloudflare.LoadFromEnv()
	if err != nil {
		slog.Error("load Cloudflare accounts", "error", err)
		os.Exit(1)
	}
	slog.Info("Cloudflare accounts loaded", "accounts", registry.Names())

	k8sCfg, err := rest.InClusterConfig()
	if err != nil {
		slog.Error("k8s config", "error", err)
		os.Exit(1)
	}

	reconciler, err := controller.New(registry, k8sCfg)
	if err != nil {
		slog.Error("init reconciler", "error", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		slog.Info("shutting down")
		cancel()
	}()

	intervalSec := envInt("RECONCILE_INTERVAL", 30)
	reconciler.Run(ctx, time.Duration(intervalSec)*time.Second)
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	var i int
	fmt.Sscanf(v, "%d", &i)
	if i <= 0 {
		return def
	}
	return i
}
```

Note: `requireEnv` helper is removed — credential validation is now done inside `cloudflare.LoadFromEnv()`.

- [ ] **Step 2: Commit (will not compile until Task 4 is done)**

```bash
git add cmd/operator/main.go
git commit -m "feat(main): use AccountRegistry instead of single-account env vars"
```

---

## Task 4: Refactor `internal/controller/reconciler.go`

**Files:**
- Modify: `internal/controller/reconciler.go`

This is the largest change. Replace the single-account `Config`/`*Client` with an `*AccountRegistry`, add `groupByAccount()`, and thread explicit `*AccountConfig`/`*Client` through each reconcile method.

- [ ] **Step 1: Rewrite `reconciler.go`**

Replace the entire file content:

```go
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Pzharyuk/cloudflare-operator/internal/cloudflare"
	"github.com/Pzharyuk/cloudflare-operator/internal/crd"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// Reconciler watches TunnelIngress CRDs and keeps Cloudflare tunnel config,
// DNS records, and Access policies in sync. It supports multiple Cloudflare
// accounts via an AccountRegistry — each TunnelIngress is processed through
// the account selected by spec.account or hostname domain matching.
type Reconciler struct {
	registry  *cloudflare.AccountRegistry
	dynClient dynamic.Interface
	gvr       schema.GroupVersionResource
	publicIP  string
	mu        sync.Mutex
}

// New creates a Reconciler using the given AccountRegistry for Cloudflare API calls.
func New(registry *cloudflare.AccountRegistry, k8sCfg *rest.Config) (*Reconciler, error) {
	dynClient, err := dynamic.NewForConfig(k8sCfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}

	return &Reconciler{
		registry:  registry,
		dynClient: dynClient,
		gvr: schema.GroupVersionResource{
			Group:    crd.Group,
			Version:  crd.Version,
			Resource: "tunnelingresses",
		},
	}, nil
}

func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	slog.Info("reconciler starting", "interval", interval, "accounts", r.registry.Names())

	// Initial reconcile
	r.reconcile(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("reconciler stopped")
			return
		case <-ticker.C:
			r.reconcile(ctx)
		}
	}
}

func (r *Reconciler) reconcile(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("panic in reconcile", "error", rec)
		}
	}()

	// List all TunnelIngress resources across namespaces
	list, err := r.dynClient.Resource(r.gvr).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		slog.Error("list TunnelIngress resources", "error", err)
		return
	}

	// Parse into typed objects
	var ingresses []crd.TunnelIngress
	for _, item := range list.Items {
		var ti crd.TunnelIngress
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, &ti); err != nil {
			slog.Error("parse TunnelIngress", "name", item.GetName(), "error", err)
			continue
		}
		ingresses = append(ingresses, ti)
	}

	slog.Info("reconciling", "ingressCount", len(ingresses))

	// Detect public IP change
	r.mu.Lock()
	if ip, err := cloudflare.GetPublicIP(); err == nil && ip != r.publicIP {
		slog.Info("public IP changed", "old", r.publicIP, "new", ip)
		r.publicIP = ip
	}
	currentIP := r.publicIP
	r.mu.Unlock()

	// Group ingresses by resolved account, then reconcile each account independently.
	// This ensures tunnel configs, DNS records, and Access policies are scoped to
	// the correct Cloudflare account for each ingress.
	groups := r.groupByAccount(ingresses)
	for accountName, accountIngresses := range groups {
		cfg, cf, err := r.registry.Get(accountName)
		if err != nil {
			slog.Error("get account for reconcile", "account", accountName, "error", err)
			continue
		}
		r.reconcileTunnel(accountIngresses, cfg, cf)
		r.reconcileDNS(accountIngresses, cfg, cf)
		r.reconcileAccess(accountIngresses, cfg, cf, currentIP)
	}

	r.updateStatuses(ctx, ingresses, currentIP)
}

// groupByAccount resolves the Cloudflare account for each TunnelIngress and groups
// them by account name. Ingresses that cannot be resolved are skipped with a warning.
//
// Resolution order (per registry.Resolve):
//  1. spec.account field (explicit)
//  2. Hostname domain suffix matching against account ZoneDomains
//  3. Default account fallback
func (r *Reconciler) groupByAccount(ingresses []crd.TunnelIngress) map[string][]crd.TunnelIngress {
	groups := make(map[string][]crd.TunnelIngress)
	for _, ti := range ingresses {
		accountName, err := r.registry.Resolve(ti.Spec.Account, ti.Spec.Hostname)
		if err != nil {
			slog.Warn("skipping ingress: cannot resolve account",
				"name", ti.Name, "namespace", ti.Namespace,
				"hostname", ti.Spec.Hostname, "error", err)
			continue
		}
		groups[accountName] = append(groups[accountName], ti)
	}
	return groups
}

// reconcileTunnel pushes the full tunnel ingress rule set for one account's ingresses.
// Existing rules not represented by a TunnelIngress are removed (declarative).
func (r *Reconciler) reconcileTunnel(ingresses []crd.TunnelIngress, cfg *cloudflare.AccountConfig, cf *cloudflare.Client) {
	rules := make([]cloudflare.TunnelIngressRule, 0, len(ingresses)+1)
	for _, ti := range ingresses {
		originReq := map[string]any{}
		if strings.HasPrefix(ti.Spec.Service, "https://") {
			originReq["noTLSVerify"] = true
		}
		rules = append(rules, cloudflare.TunnelIngressRule{
			Hostname:      ti.Spec.Hostname,
			Service:       ti.Spec.Service,
			OriginRequest: originReq,
		})
	}
	// Sort for deterministic ordering
	sort.Slice(rules, func(i, j int) bool { return rules[i].Hostname < rules[j].Hostname })
	// Catch-all must be last
	rules = append(rules, cloudflare.TunnelIngressRule{Service: "http_status:404"})

	config := &cloudflare.TunnelConfig{Ingress: rules}
	if err := cf.PutTunnelConfig(cfg.TunnelID, config); err != nil {
		slog.Error("update tunnel config", "account", cfg.Name, "tunnelID", cfg.TunnelID, "error", err)
	}
}

// reconcileDNS ensures a CNAME DNS record exists for each ingress hostname.
func (r *Reconciler) reconcileDNS(ingresses []crd.TunnelIngress, cfg *cloudflare.AccountConfig, cf *cloudflare.Client) {
	for _, ti := range ingresses {
		proxied := true
		if ti.Spec.DNS != nil && ti.Spec.DNS.Proxied != nil {
			proxied = *ti.Spec.DNS.Proxied
		}
		if err := cf.EnsureDNSRecord(cfg.ZoneID, ti.Spec.Hostname, cfg.TunnelID, proxied); err != nil {
			slog.Error("ensure DNS record", "account", cfg.Name, "hostname", ti.Spec.Hostname, "error", err)
		}
	}
}

// reconcileAccess syncs the Cloudflare Access app domains and bypass policy for
// ingresses that have Access enabled. No-ops if the account has no AccessAppID.
func (r *Reconciler) reconcileAccess(ingresses []crd.TunnelIngress, cfg *cloudflare.AccountConfig, cf *cloudflare.Client, publicIP string) {
	if cfg.AccessAppID == "" {
		return
	}

	// Collect hostnames that need Access protection and IPs for the bypass policy
	var accessDomains []string
	var bypassIPs []string
	needsBypass := false

	if publicIP != "" {
		bypassIPs = append(bypassIPs, publicIP)
	}

	for _, ti := range ingresses {
		if ti.Spec.Access != nil && ti.Spec.Access.Enabled {
			accessDomains = append(accessDomains, ti.Spec.Hostname)
			if ti.Spec.Access.BypassPublicIP {
				needsBypass = true
			}
			bypassIPs = append(bypassIPs, ti.Spec.Access.AdditionalBypassIPs...)
		}
	}

	if len(accessDomains) == 0 {
		return
	}

	sort.Strings(accessDomains)

	if err := cf.EnsureAccessAppDomains(cfg.AccessAppID, accessDomains); err != nil {
		slog.Error("update Access app domains", "account", cfg.Name, "error", err)
	}

	if needsBypass && len(bypassIPs) > 0 {
		// Deduplicate and normalize IPs to CIDR notation
		seen := make(map[string]bool)
		var uniqueIPs []string
		for _, ip := range bypassIPs {
			if !strings.Contains(ip, "/") {
				ip = ip + "/32"
			}
			if !seen[ip] {
				seen[ip] = true
				uniqueIPs = append(uniqueIPs, ip)
			}
		}
		sort.Strings(uniqueIPs)

		if err := cf.EnsureAccessBypassPolicy(cfg.AccessAppID, uniqueIPs); err != nil {
			slog.Error("update Access bypass policy", "account", cfg.Name, "error", err)
		}
	}
}

func (r *Reconciler) updateStatuses(ctx context.Context, ingresses []crd.TunnelIngress, publicIP string) {
	now := metav1.Now()
	for _, ti := range ingresses {
		status := map[string]any{
			"ready":            true,
			"tunnelConfigured": true,
			"dnsConfigured":    true,
			"publicIP":         publicIP,
			"lastSyncTime":     now.Format(time.RFC3339),
			"message":          "synced",
		}
		if ti.Spec.Access != nil && ti.Spec.Access.Enabled {
			status["accessConfigured"] = true
		}

		obj, err := r.dynClient.Resource(r.gvr).Namespace(ti.Namespace).Get(ctx, ti.Name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		obj.Object["status"] = status
		_, err = r.dynClient.Resource(r.gvr).Namespace(ti.Namespace).UpdateStatus(ctx, obj, metav1.UpdateOptions{})
		if err != nil {
			slog.Debug("update status", "name", ti.Name, "error", err)
		}
	}
}
```

- [ ] **Step 2: Build the entire project**

```bash
cd /path/to/repo && go build ./...
```

Expected: no output (success). If there are errors, fix them before committing.

- [ ] **Step 3: Commit**

```bash
git add internal/controller/reconciler.go
git commit -m "feat(controller): support multi-account via AccountRegistry; group ingresses by account"
```

---

## Task 5: Build verification and push

- [ ] **Step 1: Final clean build**

```bash
cd /path/to/repo && go build ./...
```

Expected: no errors.

- [ ] **Step 2: Verify vet passes**

```bash
cd /path/to/repo && go vet ./...
```

Expected: no output.

- [ ] **Step 3: Push branch**

```bash
git push -u origin claude/exciting-swartz
```

- [ ] **Step 4: Create PR**

```bash
gh pr create \
  --title "feat: multi-account Cloudflare support" \
  --body "$(cat <<'EOF'
## Summary

- Adds `AccountRegistry` in `internal/cloudflare/accounts.go` that reads per-account credentials from `CF_ACCOUNT_<NAME>_*` env vars
- Legacy single-account env vars (`CF_API_TOKEN`, etc.) continue to work unchanged, treated as account name `default`
- `TunnelIngressSpec` gains an optional `account` field; CRD YAML schema updated accordingly
- Reconciler groups TunnelIngress objects by resolved account and runs separate tunnel/DNS/Access reconcile passes per account
- Account selection: explicit `spec.account` → hostname domain suffix match → default fallback

## Env var format (new)

```
CF_ACCOUNT_HOME_API_TOKEN=xxx
CF_ACCOUNT_HOME_ACCOUNT_ID=yyy
CF_ACCOUNT_HOME_TUNNEL_ID=zzz
CF_ACCOUNT_HOME_ZONE_ID=aaa
CF_ACCOUNT_HOME_ZONE_DOMAIN=onit.systems   # optional, for hostname matching
CF_ACCOUNT_HOME_ACCESS_APP_ID=bbb          # optional

CF_ACCOUNT_WORK_API_TOKEN=xxx2
...
CF_DEFAULT_ACCOUNT=home                    # optional default fallback
```

## Backward compatibility

Existing deployments using `CF_API_TOKEN` / `CF_ACCOUNT_ID` / `CF_TUNNEL_ID` / `CF_ZONE_ID` require no changes — these are loaded as the `default` account and behave identically to before.

## Test plan

- [ ] Deploy with legacy env vars only → verify existing TunnelIngress CRs reconcile normally
- [ ] Add a second account via `CF_ACCOUNT_WORK_*` → deploy a TunnelIngress with `spec.account: work` → verify it routes to the correct tunnel/zone
- [ ] Deploy a TunnelIngress with no `spec.account` and `CF_ACCOUNT_HOME_ZONE_DOMAIN=onit.systems` → verify hostname `app.onit.systems` auto-routes to the `home` account
- [ ] Verify `go build ./...` and `go vet ./...` pass

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

---

## Self-Review Against Spec

**Spec requirement → Task coverage:**

| Requirement | Covered by |
|-------------|------------|
| Add `account` field to TunnelIngress spec | Task 1 |
| Update CRD manifest (`deploy/crd.yaml`) | Task 1 |
| Multi-account credential loading with env var convention | Task 2 |
| Backward compat: legacy single env vars → "default" account | Task 2 |
| Support creating multiple client instances | Task 2 (`LoadFromEnv` creates one `*Client` per account) |
| Reconciler picks client by ingress's domain/account | Task 4 (`groupByAccount` + `registry.Resolve`) |
| If `account` field set, use that; else match by domain | Task 2 (`Resolve` method, Task 4 uses it) |
| `go build ./...` passes | Task 5 |
| Create PR | Task 5 |
| Keep code style consistent (hand-rolled, slog, no controller-runtime) | All tasks |
| Comments explaining multi-account logic | Task 2 (`accounts.go` doc comments), Task 4 (inline comments) |

**No gaps found.**
