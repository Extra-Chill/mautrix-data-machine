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
	// Callback URL where the bridge listens for webhook pushes from WordPress.
	// If empty, only polling is used.
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
	return nil
}

func (dc *DataMachineConnector) GetConfig() (string, interface{}, up.Upgrader) {
	return ExampleConfig, &dc.Config, up.SimpleUpgrader(upgradeConfig)
}

func upgradeConfig(helper up.Helper) {
	helper.Copy(up.Str, "displayname_template")
	helper.Copy(up.Str, "poll_interval")
	helper.Copy(up.Str, "request_timeout")
	helper.Copy(up.Str, "callback_url")
	helper.Copy(up.Int, "callback_port")
}
