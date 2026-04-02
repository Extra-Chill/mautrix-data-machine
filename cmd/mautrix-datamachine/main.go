package main

import (
	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"

	"go.mau.fi/mautrix-datamachine/pkg/connector"
)

var (
	Tag       = "unknown"
	Commit    = "unknown"
	BuildTime = "unknown"
)

var c = &connector.DataMachineConnector{}
var m = mxmain.BridgeMain{
	Name:        "mautrix-datamachine",
	Description: "A Matrix-Data Machine chat bridge for Beeper",
	URL:         "https://github.com/Extra-Chill/mautrix-data-machine",
	Version:     "0.1.0",
	SemCalVer:   false,
	Connector:   c,
}

func main() {
	m.InitVersion(Tag, Commit, BuildTime)
	m.Run()
}
