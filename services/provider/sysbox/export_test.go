package sysbox

// SetCgroupRoot points a provider at a cgroup tree a test owns, so a unit test reads why a sandbox died, and a reopen confirms PID 1, without root.
func (p *Provider) SetCgroupRoot(root string) {
	p.cgroupRoot = root
}

// SetProcRoot points a provider at a directory that stands in for /proc, so a reopen finds the fd 0 a test placed.
func (p *Provider) SetProcRoot(root string) {
	p.procRoot = root
}
