package connector

import (
	_ "embed"
	"time"

	up "go.mau.fi/util/configupgrade"
)

//go:embed example-config.yaml
var ExampleConfig string

type Config struct {
	// How often to poll WordPress for pending messages.
	PollInterval time.Duration `yaml:"poll_interval"`
	// Timeout for HTTP requests to the WordPress site.
	RequestTimeout time.Duration `yaml:"request_timeout"`
	// Displayname template for Data Machine agents.
	DisplaynameTemplate string `yaml:"displayname_template"`
	// Default site URL shown in login prompts.
	DefaultSiteURL string `yaml:"default_site_url"`
	// Agent slug to authorize via browser login.
	AgentSlug string `yaml:"agent_slug"`
	// Callback URL where the bridge listens for webhook pushes from WordPress.
	// Also used as the PKCE redirect URI.
	CallbackURL string `yaml:"callback_url"`
	// Port for the callback listener (if callback_url is set).
	CallbackPort int `yaml:"callback_port"`
}

type umConfig Config

func (c *Config) UnmarshalYAML(unmarshal func(interface{}) error) error {
	err := unmarshal((*umConfig)(c))
	if err != nil {
		return err
	}
	if c.PollInterval == 0 {
		c.PollInterval = 5 * time.Second
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = 30 * time.Second
	}
	if c.DisplaynameTemplate == "" {
		c.DisplaynameTemplate = "{{.Name}}"
	}
	if c.AgentSlug == "" {
		c.AgentSlug = "roadie"
	}
	if c.CallbackPort == 0 {
		c.CallbackPort = 29340
	}
	return nil
}

func (dc *DataMachineConnector) GetConfig() (string, interface{}, up.Upgrader) {
	return ExampleConfig, &dc.Config, up.SimpleUpgrader(upgradeConfig)
}

func upgradeConfig(helper up.Helper) {
	helper.Copy(up.Str, "displayname_template")
	helper.Copy(up.Str, "poll_interval")
	helper.Copy(up.Str, "request_timeout")
	helper.Copy(up.Str, "default_site_url")
	helper.Copy(up.Str, "agent_slug")
	helper.Copy(up.Str, "callback_url")
	helper.Copy(up.Int, "callback_port")
}
