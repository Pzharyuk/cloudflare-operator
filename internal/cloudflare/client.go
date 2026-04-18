package cloudflare

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	apiToken  string
	accountID string
	http      *http.Client
}

func NewClient(apiToken, accountID string) *Client {
	return &Client{
		apiToken:  apiToken,
		accountID: accountID,
		http:      &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) request(method, url string, body interface{}) (json.RawMessage, error) {
	var bodyReader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		bodyReader = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var result struct {
		Success bool            `json:"success"`
		Result  json.RawMessage `json:"result"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w (body: %s)", err, string(respBody))
	}
	if !result.Success {
		msgs := make([]string, len(result.Errors))
		for i, e := range result.Errors {
			msgs[i] = fmt.Sprintf("[%d] %s", e.Code, e.Message)
		}
		return nil, fmt.Errorf("API error: %s", strings.Join(msgs, "; "))
	}
	return result.Result, nil
}

// --- Tunnel Management ---

// Tunnel represents a Cloudflare Tunnel resource.
type Tunnel struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // e.g. "inactive", "healthy", "degraded"
	Token  string `json:"token"`  // JWT token; only populated by CreateTunnel (not by List/Get)
}

// CreateTunnel creates a new named tunnel under this account.
// A random 32-byte secret is generated to register the tunnel with Cloudflare.
// The returned Tunnel.Token is the JWT cloudflared needs to authenticate — it is only
// returned at creation time, so the caller must persist it immediately.
func (c *Client) CreateTunnel(name string) (*Tunnel, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("generate tunnel secret: %w", err)
	}
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel", c.accountID)
	body := map[string]any{
		"name":          name,
		"tunnel_secret": base64.StdEncoding.EncodeToString(secret),
	}
	data, err := c.request("POST", url, body)
	if err != nil {
		return nil, err
	}
	var tunnel Tunnel
	json.Unmarshal(data, &tunnel)
	slog.Info("tunnel created", "name", tunnel.Name, "id", tunnel.ID)
	return &tunnel, nil
}

// ListTunnels returns all active (non-deleted) tunnels for this account.
// Note: the Token field is not included in list results — only CreateTunnel returns a token.
func (c *Client) ListTunnels() ([]Tunnel, error) {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel?is_deleted=false", c.accountID)
	data, err := c.request("GET", url, nil)
	if err != nil {
		return nil, err
	}
	var tunnels []Tunnel
	json.Unmarshal(data, &tunnels)
	return tunnels, nil
}

// GetTunnel retrieves a specific tunnel by ID.
func (c *Client) GetTunnel(tunnelID string) (*Tunnel, error) {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel/%s", c.accountID, tunnelID)
	data, err := c.request("GET", url, nil)
	if err != nil {
		return nil, err
	}
	var tunnel Tunnel
	json.Unmarshal(data, &tunnel)
	return &tunnel, nil
}

// DeleteTunnel deletes a tunnel by ID. The tunnel must have no active connections.
func (c *Client) DeleteTunnel(tunnelID string) error {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel/%s", c.accountID, tunnelID)
	_, err := c.request("DELETE", url, nil)
	if err == nil {
		slog.Info("tunnel deleted", "id", tunnelID)
	}
	return err
}

// --- Tunnel Ingress Config ---

type TunnelIngressRule struct {
	Hostname      string         `json:"hostname,omitempty"`
	Service       string         `json:"service"`
	OriginRequest map[string]any `json:"originRequest,omitempty"`
}

type TunnelConfig struct {
	Ingress []TunnelIngressRule `json:"ingress"`
}

func (c *Client) GetTunnelConfig(tunnelID string) (*TunnelConfig, error) {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel/%s/configurations", c.accountID, tunnelID)
	data, err := c.request("GET", url, nil)
	if err != nil {
		return nil, err
	}
	var result struct {
		Config TunnelConfig `json:"config"`
	}
	json.Unmarshal(data, &result)
	return &result.Config, nil
}

func (c *Client) PutTunnelConfig(tunnelID string, config *TunnelConfig) error {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/cfd_tunnel/%s/configurations", c.accountID, tunnelID)
	body := map[string]any{"config": map[string]any{"ingress": config.Ingress}}
	_, err := c.request("PUT", url, body)
	if err != nil {
		return err
	}
	slog.Info("tunnel config updated", "rules", len(config.Ingress)-1)
	return nil
}

// --- DNS ---

type DNSRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl"`
}

func (c *Client) ListDNSRecords(zoneID, name string) ([]DNSRecord, error) {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?name=%s", zoneID, name)
	data, err := c.request("GET", url, nil)
	if err != nil {
		return nil, err
	}
	var records []DNSRecord
	json.Unmarshal(data, &records)
	return records, nil
}

func (c *Client) EnsureDNSRecord(zoneID, hostname, tunnelID string, proxied bool) error {
	content := fmt.Sprintf("%s.cfargotunnel.com", tunnelID)
	records, err := c.ListDNSRecords(zoneID, hostname)
	if err != nil {
		return err
	}

	record := DNSRecord{Type: "CNAME", Name: hostname, Content: content, Proxied: proxied, TTL: 1}

	if len(records) > 0 {
		existing := records[0]
		if existing.Content == content && existing.Proxied == proxied {
			return nil
		}
		url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", zoneID, existing.ID)
		_, err = c.request("PUT", url, record)
		if err == nil {
			slog.Info("DNS record updated", "name", hostname)
		}
		return err
	}

	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records", zoneID)
	_, err = c.request("POST", url, record)
	if err == nil {
		slog.Info("DNS record created", "name", hostname)
	}
	return err
}

func (c *Client) DeleteDNSRecordByName(zoneID, hostname string) error {
	records, err := c.ListDNSRecords(zoneID, hostname)
	if err != nil {
		return err
	}
	for _, r := range records {
		url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", zoneID, r.ID)
		if _, err := c.request("DELETE", url, nil); err != nil {
			return err
		}
		slog.Info("DNS record deleted", "name", hostname)
	}
	return nil
}

// --- Access ---

type AccessApp struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Domain            string   `json:"domain"`
	SelfHostedDomains []string `json:"self_hosted_domains"`
	Type              string   `json:"type"`
}

type AccessPolicy struct {
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	Decision   string       `json:"decision"`
	Include    []PolicyRule `json:"include"`
	Exclude    []PolicyRule `json:"exclude"`
	Require    []PolicyRule `json:"require"`
	Precedence int          `json:"precedence"`
}

type PolicyRule map[string]any

func (c *Client) GetAccessApp(appID string) (*AccessApp, error) {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/access/apps/%s", c.accountID, appID)
	data, err := c.request("GET", url, nil)
	if err != nil {
		return nil, err
	}
	var app AccessApp
	json.Unmarshal(data, &app)
	return &app, nil
}

func (c *Client) EnsureAccessAppDomains(appID string, domains []string) error {
	app, err := c.GetAccessApp(appID)
	if err != nil {
		return err
	}

	// Check if domains match
	existing := make(map[string]bool)
	for _, d := range app.SelfHostedDomains {
		existing[d] = true
	}
	allMatch := len(existing) == len(domains)
	for _, d := range domains {
		if !existing[d] {
			allMatch = false
			break
		}
	}
	if allMatch {
		return nil
	}

	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/access/apps/%s", c.accountID, appID)
	body := map[string]any{
		"self_hosted_domains": domains,
		"domain":             domains[0],
		"type":               app.Type,
		"name":               app.Name,
	}
	_, err = c.request("PUT", url, body)
	if err == nil {
		slog.Info("Access app domains updated", "domains", domains)
	}
	return err
}

func (c *Client) EnsureAccessBypassPolicy(appID string, ips []string) error {
	policiesURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/access/apps/%s/policies", c.accountID, appID)
	data, err := c.request("GET", policiesURL, nil)
	if err != nil {
		return err
	}
	var policies []AccessPolicy
	json.Unmarshal(data, &policies)

	includes := make([]PolicyRule, len(ips))
	for i, ip := range ips {
		if !strings.Contains(ip, "/") {
			ip = ip + "/32"
		}
		includes[i] = PolicyRule{"ip": map[string]any{"ip": ip}}
	}

	policy := map[string]any{
		"name":       "Auto-managed IP bypass",
		"decision":   "bypass",
		"include":    includes,
		"exclude":    []any{},
		"require":    []any{},
		"precedence": 1,
	}

	for _, p := range policies {
		if p.Name == "Auto-managed IP bypass" || p.Name == "Bypass internal apps" || p.Name == "Bypass internal apps to keycloak" {
			url := fmt.Sprintf("%s/%s", policiesURL, p.ID)
			_, err = c.request("PUT", url, policy)
			if err == nil {
				slog.Info("Access bypass policy updated", "ips", ips)
			}
			return err
		}
	}

	_, err = c.request("POST", policiesURL, policy)
	if err == nil {
		slog.Info("Access bypass policy created", "ips", ips)
	}
	return err
}

// --- Public IP ---

func GetPublicIP() (string, error) {
	resp, err := http.Get("https://ifconfig.me/ip")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(body)), nil
}
