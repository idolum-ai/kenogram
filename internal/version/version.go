package version

import "runtime"

var Version = "dev"
var Commit = "unknown"
var Date = "unknown"

func String() string {
	return "kenogram " + Version + " commit=" + Commit + " date=" + Date + " go=" + runtime.Version()
}
