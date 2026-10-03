package runc

// SetCgroupRoot points a provider at a cgroup tree a test owns, so a unit test reads why a sandbox died without root.
func (p *Provider) SetCgroupRoot(root string) {
	p.cgroupRoot = root
}
