package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"

	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/jetstack/cert-manager/pkg/acme/webhook/apis/acme/v1alpha1"
	"github.com/jetstack/cert-manager/pkg/acme/webhook/cmd"
	certmgrv1 "github.com/jetstack/cert-manager/pkg/apis/meta/v1"
	"github.com/jetstack/cert-manager/pkg/issuer/acme/dns/util"

	pkgutil "github.com/jetstack/cert-manager/pkg/util"
)

const providerName = "godaddy"

// GroupName a API group name
var GroupName = os.Getenv("GROUP_NAME")

// DNSRecord a DNS record
type DNSRecord struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Data     string `json:"data"`
	Priority int    `json:"priority,omitempty"`
	TTL      int    `json:"ttl,omitempty"`
}

func main() {
	if GroupName == "" {
		panic("GROUP_NAME must be specified")
	}

	// This will register our godaddy DNS provider with the webhook serving
	// library, making it available as an API under the provided GroupName.
	// You can register multiple DNS provider implementations with a single
	// webhook, where the Name() method will be used to disambiguate between
	// the different implementations.
	cmd.RunWebhookServer(GroupName,
		&godaddyDNSSolver{},
	)
}

// godaddyDNSSolver implements the provider-specific logic needed to
// 'present' an ACME challenge TXT record for your own DNS provider.
// To do so, it must implement the `github.com/jetstack/cert-manager/pkg/acme/webhook.Solver`
// interface.
type godaddyDNSSolver struct {
	client *kubernetes.Clientset
}

// godaddyDNSProviderConfig is a structure that is used to decode into when
// solving a DNS01 challenge.
// This information is provided by cert-manager, and may be a reference to
// additional configuration that's needed to solve the challenge for this
// particular certificate or issuer.
// This typically includes references to Secret resources containing DNS
// provider credentials, in cases where a 'multi-tenant' DNS solver is being
// created.
// If you do *not* require per-issuer or per-certificate configuration to be
// provided to your webhook, you can skip decoding altogether in favour of
// using CLI flags or similar to provide configuration.
// You should not include sensitive information here. If credentials need to
// be used by your provider here, you should reference a Kubernetes Secret
// resource and fetch these credentials using a Kubernetes clientset.
type godaddyDNSProviderConfig struct {
	// These fields will be set by users in the
	// `issuer.spec.acme.dns01.providers.webhook.config` field.

	APIKeyRef certmgrv1.SecretKeySelector `json:"apiKeyRef"`
	APISecretRef certmgrv1.SecretKeySelector `json:"apiSecretRef"`

	AuthAPIKey    string `json:"authApiKey"`
	AuthAPISecret string `json:"authApiSecret"`
	Production    bool   `json:"production"`

	// +optional. The TTL of the TXT record used for the DNS challenge
	TTL           int    `json:"ttl"`
	// +optional.  API request timeout
	HttpTimeout int `json:"timeout"`
	// +optional.  Maximum waiting time for DNS propagation
	PropagationTimeout int `json:"propagationTimeout"`
	// +optional. Time between DNS propagation check
	PollingInterval int `json:"pollingInterval"`
	// +optional. Interval between iteration
	SequenceInterval int `json:"sequenceInterval"`
}

func (c *godaddyDNSSolver) validate(cfg *godaddyDNSProviderConfig) error {
	// Try to load the API key
	if cfg.APIKeyRef.LocalObjectReference.Name == "" || cfg.APISecretRef.LocalObjectReference.Name == "" {
		return errors.New("API token field were not provided as no Kubernetes Secret exists !")
	}
	return nil
}

// Name is used as the name for this DNS solver when referencing it on the ACME
// Issuer resource.
// This should be unique **within the group name**, i.e. you can have two
// solvers configured with the same Name() **so long as they do not co-exist
// within a single webhook deployment**.
// For example, `cloudflare` may be used as the name of a solver.
func (c *godaddyDNSSolver) Name() string {
	return providerName
}

// Return GoDaddi API URL to query the API domains
// See - https://developer.godaddy.com/doc/endpoint/domains
// OTE environment: https://api.ote-godaddy.com
// PRODUCTION environment: https://api.godaddy.com
func (c *godaddyDNSSolver) apiURL(cfg godaddyDNSProviderConfig) string {
	baseURL := "https://api.ote-godaddy.com"
	if cfg.Production {
		baseURL = "https://api.godaddy.com"
	}
	return baseURL
}

func (c *godaddyDNSSolver) extractApiTokenFromSecret(cfg *godaddyDNSProviderConfig, ch *v1alpha1.ChallengeRequest) error {
	keySec, err := c.client.CoreV1().
		Secrets(ch.ResourceNamespace).
		Get(cfg.APIKeyRef.LocalObjectReference.Name, metaV1.GetOptions{})
	if err != nil {
		return err
	}

	keySecBytes, ok := keySec.Data[cfg.APIKeyRef.Key]
	if !ok {
		return fmt.Errorf("Key %q not found in secret \"%s/%s\"",
			cfg.APIKeyRef.Key,
			cfg.APIKeyRef.LocalObjectReference.Name,
			ch.ResourceNamespace)
	}

	cfg.AuthAPIKey = string(keySecBytes)

	secSec, err := c.client.CoreV1().
		Secrets(ch.ResourceNamespace).
		Get(cfg.APISecretRef.LocalObjectReference.Name, metaV1.GetOptions{})
	if err != nil {
		return err
	}

	secSecBytes, ok := secSec.Data[cfg.APISecretRef.Key]
	if !ok {
		return fmt.Errorf("Key %q not found in secret \"%s/%s\"",
			cfg.APISecretRef.Key,
			cfg.APISecretRef.LocalObjectReference.Name,
			ch.ResourceNamespace)
	}

	cfg.AuthAPISecret = string(secSecBytes)

	return nil
}

// Present is responsible for actually presenting the DNS record with the
// DNS provider.
// This method should tolerate being called multiple times with the same value.
// cert-manager itself will later perform a self check to ensure that the
// solver has correctly configured the DNS provider.
func (c *godaddyDNSSolver) Present(ch *v1alpha1.ChallengeRequest) error {
	cfg, err := loadConfig(ch.Config)
	if err != nil {
		return err
	}

	// Verify if the config contains the required parameters such as SecretRef
	if err := c.validate(&cfg); err != nil {
		return err
	}

	// Extract the Godaddy Api and Secret from the K8s Secret
	// and assign it the AuthAPIKey and AuthAPISecret of the Config
	if err := c.extractApiTokenFromSecret(&cfg, ch); err != nil {
		return err
	}

	baseURL := c.apiURL(cfg)

	recordName := c.extractRecordName(ch.ResolvedFQDN, ch.ResolvedZone)

	dnsZone, err := c.getZone(ch.ResolvedZone)
	if err != nil {
		return err
	}

	rec := []DNSRecord{
		{
			Type: "TXT",
			Name: recordName,
			Data: ch.Key,
			TTL:  cfg.TTL,
		},
	}

	return c.updateRecords(cfg, baseURL, rec, dnsZone, recordName)
}

// CleanUp should delete the relevant TXT record from the DNS provider console.
// If multiple TXT records exist with the same record name (e.g.
// _acme-challenge.example.com) then **only** the record with the same `key`
// value provided on the ChallengeRequest should be cleaned up.
// This is in order to facilitate multiple DNS validations for the same domain
// concurrently.
func (c *godaddyDNSSolver) CleanUp(ch *v1alpha1.ChallengeRequest) error {
	cfg, err := loadConfig(ch.Config)
	if err != nil {
		return err
	}

	// Verify if the config contains the required parameters such as SecretRef
	if err := c.validate(&cfg); err != nil {
		return err
	}

	// Extract the Godaddy Api and Secret from the K8s Secret
	// and assign it the AuthAPIKey and AuthAPISecret of the Config
	if err := c.extractApiTokenFromSecret(&cfg, ch); err != nil {
		return err
	}

	baseURL := c.apiURL(cfg)

	recordName := c.extractRecordName(ch.ResolvedFQDN, ch.ResolvedZone)

	dnsZone, err := c.getZone(ch.ResolvedZone)
	if err != nil {
		return err
	}

	rec := []DNSRecord{
		{
			Type: "TXT",
			Name: recordName,
			Data: "null",
		},
	}

	return c.updateRecords(cfg, baseURL, rec, dnsZone, recordName)
}

// Initialize will be called when the webhook first starts.
// This method can be used to instantiate the webhook, i.e. initialising
// connections or warming up caches.
// Typically, the kubeClientConfig parameter is used to build a Kubernetes
// client that can be used to fetch resources from the Kubernetes API, e.g.
// Secret resources containing credentials used to authenticate with DNS
// provider accounts.
// The stopCh can be used to handle early termination of the webhook, in cases
// where a SIGTERM or similar signal is sent to the webhook process.
func (c *godaddyDNSSolver) Initialize(kubeClientConfig *rest.Config, stopCh <-chan struct{}) error {
	cl, err := kubernetes.NewForConfig(kubeClientConfig)
	if err != nil {
		return err
	}

	c.client = cl
	return nil
}

// loadConfig is a small helper function that decodes JSON configuration into
// the typed config struct.
func loadConfig(cfgJSON *apiext.JSON) (godaddyDNSProviderConfig, error) {
	cfg := godaddyDNSProviderConfig{}
	// handle the 'base case' where no configuration has been provided
	if cfgJSON == nil {
		return cfg, nil
	}
	if err := json.Unmarshal(cfgJSON.Raw, &cfg); err != nil {
		return cfg, fmt.Errorf("error decoding solver config: %v", err)
	}

	return cfg, nil
}

func (c *godaddyDNSSolver) updateRecords(cfg godaddyDNSProviderConfig, baseURL string, records []DNSRecord, domainZone string, recordName string) error {
	body, err := json.Marshal(records)
	if err != nil {
		return err
	}

	var resp *http.Response
	url := fmt.Sprintf("/v1/domains/%s/records/TXT/%s", domainZone, recordName)
	resp, err = c.makeRequest(cfg, baseURL, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("could not create record %v; Status: %v; Body: %s", string(body), resp.StatusCode, string(bodyBytes))
	}
	return nil
}

func (c *godaddyDNSSolver) makeRequest(cfg godaddyDNSProviderConfig, baseURL string, method string, uri string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, fmt.Sprintf("%s%s", baseURL, uri), body)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", pkgutil.CertManagerUserAgent)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("sso-key %s:%s", cfg.AuthAPIKey, cfg.AuthAPISecret))

	client := http.Client{
		Timeout: 30 * time.Second,
	}

	return client.Do(req)
}

func (c *godaddyDNSSolver) extractRecordName(fqdn, domain string) string {
	if idx := strings.Index(fqdn, "."+domain); idx != -1 {
		return fqdn[:idx]
	}
	return util.UnFqdn(fqdn)
}

func (c *godaddyDNSSolver) extractDomainName(zone string) string {
	authZone, err := util.FindZoneByFqdn(zone, util.RecursiveNameservers)
	if err != nil {
		return zone
	}
	return util.UnFqdn(authZone)
}

func (c *godaddyDNSSolver) getZone(fqdn string) (string, error) {
	authZone, err := util.FindZoneByFqdn(fqdn, util.RecursiveNameservers)
	if err != nil {
		return "", err
	}

	return util.UnFqdn(authZone), nil
}
