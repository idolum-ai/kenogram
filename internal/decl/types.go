package decl

// Declaration is schema version 1 of a Kenogram world declaration.
type Declaration struct {
	Version       int64
	Name          string
	AllowUnpinned bool
	World         World
	Resources     Resources
	Workspace     Workspace
	Copies        []Copy
	Mounts        []Mount
	Network       Network
	Interfaces    []Interface
	Services      []Service
}

type World struct {
	Hostname string
	Base     string
	Workdir  string
	User     string
}

type Resources struct {
	CPUs        int64
	MemoryBytes int64
	PIDs        int64
}

type Workspace struct {
	Paths []string
}

type Copy struct {
	Source string
	Target string
	Mode   string
	Secret bool
}

type Mount struct {
	Source string
	Target string
	Mode   string
}

type Network struct {
	Allow []NetworkAllow
}

type NetworkAllow struct {
	Host string
	Port int64
}

// Interface names an operator-facing byte stream whose listener remains on
// loopback inside the world's otherwise isolated network namespace.
type Interface struct {
	Name    string
	Address string
}

type Service struct {
	Name      string
	Command   []string
	Autostart bool
	Restart   string
}
