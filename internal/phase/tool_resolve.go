package phase

import "github.com/NoviceAtPython/CloudDeploy-mover/internal/discovery"

func resolveQDBus() (discovery.Command, error) {
	return discovery.MustQDBus(discovery.Resolver{})
}

func resolveKScreenDoctor() (discovery.Command, error) {
	return discovery.Resolver{}.ResolveCommand([]string{"kscreen-doctor"})
}

func resolveDoxygen() (discovery.Command, error) {
	return discovery.Resolver{}.ResolveCommand([]string{"/usr/local/bin/doxygen", "doxygen"})
}

func resolveNVCC() (discovery.Command, error) {
	return discovery.Resolver{}.ResolveCommand([]string{"/usr/local/cuda/bin/nvcc", "nvcc"})
}
