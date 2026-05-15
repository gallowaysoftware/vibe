package supervisor

// LaunchSpec is the backend-agnostic launch description the supervisor needs
// to exec a child process and decide when it's "ready". It is produced by a
// per-backend builder in the profile package (see profile.LlamaServerSpec /
// profile.ComfyUISpec) so the supervisor itself stays free of llama- or
// ComfyUI-specific knowledge.
type LaunchSpec struct {
	// Binary is an absolute path or a $PATH-resolvable name (e.g.
	// "llama-server", "python3", "/usr/bin/python").
	Binary string

	// Args is the argv passed to Binary, not including Binary itself.
	Args []string

	// Workdir, if non-empty, becomes the child's working directory. ComfyUI
	// expects to be launched from its checkout (so it can find `web/`,
	// `custom_nodes/`, etc.); llama-server doesn't care.
	Workdir string

	// Env, if non-empty, is appended to os.Environ() for the child. Use this
	// for things like PYTHONUNBUFFERED=1.
	Env []string

	// HealthURL is the full URL (including scheme + host + port + path) that
	// the supervisor polls until it returns 2xx, signaling the child is
	// ready. llama-server: ".../health"; ComfyUI: ".../system_stats".
	HealthURL string
}
