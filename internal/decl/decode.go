package decl

import "fmt"

func decodeDeclaration(root table) (Declaration, error) {
	r := decoder{values: root, path: "root"}
	d := Declaration{}
	var err error
	if d.Version, err = r.integer("version", true); err != nil {
		return d, err
	}
	if d.Name, err = r.string("name", true); err != nil {
		return d, err
	}
	if d.AllowUnpinned, err = r.boolean("allow_unpinned", false); err != nil {
		return d, err
	}

	w, err := r.child("world", true)
	if err != nil {
		return d, err
	}
	if d.World.Hostname, err = w.string("hostname", true); err != nil {
		return d, err
	}
	if d.World.Base, err = w.string("base", true); err != nil {
		return d, err
	}
	if d.World.Workdir, err = w.string("workdir", true); err != nil {
		return d, err
	}
	if d.World.User, err = w.string("user", true); err != nil {
		return d, err
	}
	if err = w.done(); err != nil {
		return d, err
	}

	resources, err := r.child("resources", true)
	if err != nil {
		return d, err
	}
	if d.Resources.CPUs, err = resources.integer("cpus", true); err != nil {
		return d, err
	}
	if d.Resources.MemoryBytes, err = resources.integer("memory_bytes", true); err != nil {
		return d, err
	}
	if d.Resources.PIDs, err = resources.integer("pids", true); err != nil {
		return d, err
	}
	if err = resources.done(); err != nil {
		return d, err
	}

	workspace, err := r.child("workspace", true)
	if err != nil {
		return d, err
	}
	if d.Workspace.Paths, err = workspace.strings("paths", true); err != nil {
		return d, err
	}
	if err = workspace.done(); err != nil {
		return d, err
	}

	copyItems, err := r.children("copies")
	if err != nil {
		return d, err
	}
	for _, item := range copyItems {
		var c Copy
		if c.Source, err = item.string("source", true); err != nil {
			return d, err
		}
		if c.Target, err = item.string("target", true); err != nil {
			return d, err
		}
		if c.Mode, err = item.string("mode", true); err != nil {
			return d, err
		}
		if c.Secret, err = item.boolean("secret", false); err != nil {
			return d, err
		}
		if err = item.done(); err != nil {
			return d, err
		}
		d.Copies = append(d.Copies, c)
	}

	mountItems, err := r.children("mounts")
	if err != nil {
		return d, err
	}
	for _, item := range mountItems {
		var m Mount
		if m.Source, err = item.string("source", true); err != nil {
			return d, err
		}
		if m.Target, err = item.string("target", true); err != nil {
			return d, err
		}
		if m.Mode, err = item.string("mode", true); err != nil {
			return d, err
		}
		if err = item.done(); err != nil {
			return d, err
		}
		d.Mounts = append(d.Mounts, m)
	}

	network, err := r.child("network", false)
	if err != nil {
		return d, err
	}
	if network.values != nil {
		allowItems, err := network.children("allow")
		if err != nil {
			return d, err
		}
		for _, item := range allowItems {
			var a NetworkAllow
			if a.Host, err = item.string("host", true); err != nil {
				return d, err
			}
			if a.Port, err = item.integer("port", true); err != nil {
				return d, err
			}
			if err = item.done(); err != nil {
				return d, err
			}
			d.Network.Allow = append(d.Network.Allow, a)
		}
		if err = network.done(); err != nil {
			return d, err
		}
	}

	interfaceItems, err := r.children("interfaces")
	if err != nil {
		return d, err
	}
	for _, item := range interfaceItems {
		var endpoint Interface
		if endpoint.Name, err = item.string("name", true); err != nil {
			return d, err
		}
		if endpoint.Address, err = item.string("address", true); err != nil {
			return d, err
		}
		if err = item.done(); err != nil {
			return d, err
		}
		d.Interfaces = append(d.Interfaces, endpoint)
	}

	serviceItems, err := r.children("services")
	if err != nil {
		return d, err
	}
	for _, item := range serviceItems {
		var s Service
		if s.Name, err = item.string("name", true); err != nil {
			return d, err
		}
		if s.Command, err = item.strings("command", true); err != nil {
			return d, err
		}
		if s.Autostart, err = item.boolean("autostart", false); err != nil {
			return d, err
		}
		if s.Restart, err = item.string("restart", true); err != nil {
			return d, err
		}
		if err = item.done(); err != nil {
			return d, err
		}
		d.Services = append(d.Services, s)
	}
	if err = r.done(); err != nil {
		return d, err
	}
	return d, nil
}

type decoder struct {
	values table
	path   string
}

func (d *decoder) take(key string, required bool) (any, error) {
	v, ok := d.values[key]
	if !ok {
		if required {
			return nil, fmt.Errorf("%s.%s is required", d.path, key)
		}
		return nil, nil
	}
	delete(d.values, key)
	return v, nil
}

func (d *decoder) string(key string, required bool) (string, error) {
	v, err := d.take(key, required)
	if err != nil || v == nil {
		return "", err
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s.%s must be a string", d.path, key)
	}
	return s, nil
}
func (d *decoder) integer(key string, required bool) (int64, error) {
	v, err := d.take(key, required)
	if err != nil || v == nil {
		return 0, err
	}
	n, ok := v.(int64)
	if !ok {
		return 0, fmt.Errorf("%s.%s must be an integer", d.path, key)
	}
	return n, nil
}
func (d *decoder) boolean(key string, required bool) (bool, error) {
	v, err := d.take(key, required)
	if err != nil || v == nil {
		return false, err
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("%s.%s must be a boolean", d.path, key)
	}
	return b, nil
}
func (d *decoder) strings(key string, required bool) ([]string, error) {
	v, err := d.take(key, required)
	if err != nil || v == nil {
		return nil, err
	}
	values, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s.%s must be an array", d.path, key)
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("%s.%s must contain only strings", d.path, key)
		}
		result = append(result, s)
	}
	return result, nil
}
func (d *decoder) child(key string, required bool) (decoder, error) {
	v, err := d.take(key, required)
	if err != nil || v == nil {
		return decoder{path: d.path + "." + key}, err
	}
	t, ok := v.(table)
	if !ok {
		return decoder{}, fmt.Errorf("%s.%s must be a table", d.path, key)
	}
	return decoder{values: t, path: d.path + "." + key}, nil
}
func (d *decoder) children(key string) ([]decoder, error) {
	v, err := d.take(key, false)
	if err != nil || v == nil {
		return nil, err
	}
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s.%s must be an array of tables", d.path, key)
	}
	result := make([]decoder, 0, len(items))
	for i, item := range items {
		t, ok := item.(table)
		if !ok {
			return nil, fmt.Errorf("%s.%s[%d] must be a table", d.path, key, i)
		}
		result = append(result, decoder{values: t, path: fmt.Sprintf("%s.%s[%d]", d.path, key, i)})
	}
	return result, nil
}
func (d *decoder) done() error {
	for key := range d.values {
		return fmt.Errorf("unknown key or table %s.%s", d.path, key)
	}
	return nil
}
