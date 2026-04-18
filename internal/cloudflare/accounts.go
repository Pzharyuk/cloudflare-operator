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
	// If CF_API_TOKEN is set, load it as the "default" account. This keeps existing
	// deployments working without any configuration changes.
	if token := os.Getenv("CF_API_TOKEN"); token != "" {
		accountID := os.Getenv("CF_ACCOUNT_ID")
		tunnelID := os.Getenv("CF_TUNNEL_ID") // optional — operator will auto-create if empty
		zoneID := os.Getenv("CF_ZONE_ID")
		if accountID == "" || zoneID == "" {
			return nil, fmt.Errorf("legacy account: CF_ACCOUNT_ID and CF_ZONE_ID are required when CF_API_TOKEN is set")
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
	// We detect account names by finding all env keys that match the prefix/suffix
	// pattern, then load the full credential set for each discovered name.
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
		tunnelID := os.Getenv(p + "TUNNEL_ID") // optional — operator will auto-create if empty
		zoneID := os.Getenv(p + "ZONE_ID")
		if token == "" || accountID == "" || zoneID == "" {
			return nil, fmt.Errorf("account %q: %sAPI_TOKEN, %sACCOUNT_ID, and %sZONE_ID are required",
				nameLower, p, p, p)
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

	// Determine the default fallback account.
	if explicitDefault := strings.ToLower(os.Getenv("CF_DEFAULT_ACCOUNT")); explicitDefault != "" {
		if _, ok := reg.accounts[explicitDefault]; !ok {
			return nil, fmt.Errorf("CF_DEFAULT_ACCOUNT=%q does not match any configured account; configured: %v",
				explicitDefault, reg.Names())
		}
		reg.defaultName = explicitDefault
	} else if reg.defaultName == "" {
		// No legacy account and no explicit default: auto-select if only one account.
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

// SetTunnelID updates the tunnel ID for a named account in memory.
// Called after auto-creating a tunnel at startup so the reconciler can use it.
func (r *AccountRegistry) SetTunnelID(name, tunnelID string) {
	if e, ok := r.accounts[name]; ok {
		e.cfg.TunnelID = tunnelID
	}
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
	// 1. Explicit account name from spec.account
	if accountName != "" {
		nameLower := strings.ToLower(accountName)
		if _, ok := r.accounts[nameLower]; !ok {
			return "", fmt.Errorf("spec.account=%q not found; configured accounts: %v", accountName, r.Names())
		}
		return nameLower, nil
	}

	// 2. Hostname domain suffix match against accounts with ZoneDomain configured
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
