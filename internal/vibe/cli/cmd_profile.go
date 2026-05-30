package cli

import (
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
)

// profileNameRE mirrors profile.Validate()'s allowed name shape. We
// duplicate the regex here (rather than reaching into the profile package
// for a private value) to keep the cli -> profile dependency one-way and
// to fail fast in `vibe profile init` before we ever write a file.
var profileNameRE = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// profileTemplates holds the embedded starter YAML files. Each file's name
// (without the ".yaml" suffix) is the <kind> users pass to `vibe profile
// init`. Filenames are kept in sync with profile.Backend / profile.Frontend
// kinds; add a new file here to expose a new --kind on disk.
//
//go:embed profile_templates/*.yaml
var profileTemplates embed.FS

// profileNamePlaceholder is the literal token in each template that gets
// substituted with the user-supplied --name (or the default <kind>). We use
// an unambiguous all-caps token rather than a Go text/template directive
// because the templates are also valid YAML — keeping them parsable lets us
// hand-edit them without a build step.
const profileNamePlaceholder = "__PROFILE_NAME__"

// validProfileKinds returns the kinds supported by `vibe profile init`,
// derived from the embedded filenames. The list is sorted so help/error
// output is deterministic.
func validProfileKinds() ([]string, error) {
	entries, err := fs.ReadDir(profileTemplates, "profile_templates")
	if err != nil {
		return nil, fmt.Errorf("read embedded profile templates: %w", err)
	}
	kinds := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		kinds = append(kinds, strings.TrimSuffix(name, ".yaml"))
	}
	sort.Strings(kinds)
	return kinds, nil
}

// loadProfileTemplate returns the raw template bytes for `kind`. The caller
// is responsible for substituting placeholders.
func loadProfileTemplate(kind string) (string, error) {
	body, err := profileTemplates.ReadFile(filepath.Join("profile_templates", kind+".yaml"))
	if err != nil {
		kinds, _ := validProfileKinds()
		return "", fmt.Errorf("unknown kind %q (expected one of: %s)", kind, strings.Join(kinds, ", "))
	}
	return string(body), nil
}

func profileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profile",
		Short: "Manage vibe profiles.",
		Long: "vibe profile groups commands that operate on profile YAML files " +
			"under $XDG_CONFIG_HOME/vibe/profiles/.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(profileInitCmd())
	cmd.AddCommand(profileNewCmd())
	cmd.AddCommand(profileSchemaCmd())
	cmd.AddCommand(profileShowCmd())
	cmd.AddCommand(profileListCmd())
	return cmd
}

// profileShowCmd prints the loaded + validated profile YAML for a given
// name. Resolves the on-disk path via ProfilesDir() and runs the full
// profile.Load pipeline so the output reflects any tilde-expansion,
// default-filling, or sanity-rejection the daemon would apply at
// activation time.
//
// Why this exists: during audits / debugging the user kept resorting
// to `cat ~/.config/vibe/profiles/<name>.yaml`, which (a) misses
// validation, and (b) doesn't surface expanded paths. A first-class
// command is one less mental step.
func profileShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Print the loaded + validated profile YAML for <name>.",
		Long: "show reads $XDG_CONFIG_HOME/vibe/profiles/<name>.yaml, " +
			"runs it through profile.Load (tilde expansion, defaults, " +
			"validation), and prints the result. Errors out on " +
			"missing files or validation failures so it doubles as a " +
			"lint check.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			path := filepath.Join(paths.ProfilesDir(), name+".yaml")
			if _, err := os.Stat(path); err != nil {
				return fmt.Errorf("profile %q not found at %s (run `vibe profile list` to see available)", name, path)
			}
			p, err := profile.Load(path)
			if err != nil {
				return fmt.Errorf("profile %q failed to load: %w", name, err)
			}
			raw, err := yaml.Marshal(p)
			if err != nil {
				return fmt.Errorf("marshal profile: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "# %s\n# loaded from %s\n%s",
				name, path, string(raw))
			return nil
		},
	}
}

// profileListCmd prints every profile YAML under ProfilesDir() with
// its name + description + backend kind + mode. Counterpart to
// `vibe profile show` for the "which profiles do I have" question.
// The top-level `vibe list` is a thin alias for this command — both
// share renderProfileList so the two never drift in format.
func profileListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List every profile YAML with name + description + backend + mode.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return renderProfileList(cmd.OutOrStdout())
		},
	}
}

// renderProfileList scans ProfilesDir() and prints one aligned row per
// profile YAML. Invalid files are listed with an `(invalid: ...)` note
// rather than aborting the whole listing, so one broken file doesn't hide
// the rest. A missing profiles directory is treated as "no profiles yet"
// rather than an error — the dir is created lazily by `profile new`.
func renderProfileList(out io.Writer) error {
	dir := paths.ProfilesDir()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(out, "no profiles yet (create one with `vibe profile new <name>`)")
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}

	type row struct{ name, mode, backend, desc string }
	var rows []row
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		p, err := profile.Load(filepath.Join(dir, e.Name()))
		if err != nil {
			rows = append(rows, row{
				name:    strings.TrimSuffix(e.Name(), ".yaml"),
				mode:    "?",
				backend: "?",
				desc:    fmt.Sprintf("(invalid: %v)", err),
			})
			continue
		}
		backend := "?"
		switch {
		case p.Backend.LlamaServer != nil:
			backend = "llama_server"
		case p.Backend.TabbyAPI != nil:
			backend = "tabby_api"
		case p.Backend.HTTPServer != nil:
			backend = "http_server"
		case p.Backend.ComfyUI != nil:
			backend = "comfyui"
		}
		rows = append(rows, row{p.Name, p.ResolvedMode(), backend, p.Description})
	}

	if len(rows) == 0 {
		fmt.Fprintln(out, "no profiles yet (create one with `vibe profile new <name>`)")
		return nil
	}

	fmt.Fprintf(out, "%-22s  %-10s  %-12s  %s\n", "NAME", "MODE", "BACKEND", "DESCRIPTION")
	for _, r := range rows {
		fmt.Fprintf(out, "%-22s  %-10s  %-12s  %s\n", r.name, r.mode, r.backend, r.desc)
	}
	return nil
}

// profileKinds is the user-facing --kind values for `vibe profile new`,
// derived from the bundled template filenames so every shipped starter is
// discoverable via --help and tab-completion (not just llama-server /
// comfyui). Falls back to those two if the embed read ever fails, which it
// won't in a built binary.
func profileKinds() []string {
	kinds, err := validProfileKinds()
	if err != nil || len(kinds) == 0 {
		return []string{"llama-server", "comfyui"}
	}
	return kinds
}

// profileFrontendEnum is the user-facing --frontend values. Only meaningful
// for kind=llama-server; ignored (with a friendly note) for kind=comfyui
// since ComfyUI ships its own UI and the profile schema rejects a
// frontend block on comfyui-backed profiles.
var profileFrontendEnum = []string{
	profile.FrontendExternal,
	profile.FrontendDockerCompose,
	profile.FrontendManaged,
}

// templateForKindFrontend maps the (--kind, --frontend) pair onto a
// concrete template filename under profile_templates/.
//
// For kind=llama-server the --frontend flag is sugar that selects the
// frontend-specific variant (external -> llama-server, docker-compose ->
// docker-compose, managed -> managed). For every other kind --frontend is
// ignored — those backends either ship their own UI (comfyui) or are
// service-mode sidecars with no frontend (tabby-api, http-service,
// llama-embed-service) — and the kind maps straight to the same-named
// template. Any kind matching a bundled template filename is accepted.
func templateForKindFrontend(kind, frontend string) (string, error) {
	if kind == "llama-server" {
		switch frontend {
		case profile.FrontendExternal, "":
			return "llama-server", nil
		case profile.FrontendDockerCompose:
			return "docker-compose", nil
		case profile.FrontendManaged:
			return "managed", nil
		default:
			return "", fmt.Errorf("--frontend %q: unknown (allowed: %s)",
				frontend, strings.Join(profileFrontendEnum, ", "))
		}
	}
	if slices.Contains(profileKinds(), kind) {
		return kind, nil
	}
	return "", fmt.Errorf("--kind %q: unknown (allowed: %s)",
		kind, strings.Join(profileKinds(), ", "))
}

// profileSchemaCmd emits the JSON Schema (draft-07) describing profile YAML.
// Mirrors `vamp schema` in shape: stdout-by-default with --out for writing
// the canonical file, no other options. Editor extensions point a
// `# yaml-language-server: $schema=...` directive at the rendered file to
// get autocomplete + validation on profile YAMLs.
func profileSchemaCmd() *cobra.Command {
	var outFlag string
	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Emit the vibe profile JSON Schema (draft-07).",
		Long: "schema prints a JSON Schema document describing profile YAML " +
			"to stdout (or --out <file>). The schema covers every Backend " +
			"(llama_server / comfyui) and Frontend (external / " +
			"docker-compose / managed) shape, including the huggingface " +
			"pull block and the mmproj field. Point yaml-language-server at " +
			"the rendered file with a " +
			"`# yaml-language-server: $schema=./profile.schema.json` " +
			"directive at the top of your profile YAML to get editor " +
			"validation and autocomplete.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := profile.SchemaJSON()
			if err != nil {
				return err
			}
			if outFlag == "" {
				_, err := cmd.OutOrStdout().Write(data)
				return err
			}
			if err := os.WriteFile(outFlag, data, 0o644); err != nil {
				return fmt.Errorf("write %s: %w", outFlag, err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&outFlag, "out", "",
		"Write the schema to this file instead of stdout.")
	return cmd
}

// profileNewCmd is the user-facing alternative to `vibe profile init`. The
// shape is closer to what a UNIX user expects from `<noun> new`:
// flag-driven kind selection, an explicit name positional argument, and
// shell completion on the enum-shaped flags. The internals reuse
// runProfileInit so behavior stays identical across the two entry points.
func profileNewCmd() *cobra.Command {
	var (
		kind     string
		frontend string
		hfRef    string
		force    bool
	)
	cmd := &cobra.Command{
		Use:   "new <name>",
		Short: "Create a new profile YAML under $XDG_CONFIG_HOME/vibe/profiles/.",
		Long: "Generates a starter profile YAML at " +
			"$XDG_CONFIG_HOME/vibe/profiles/<name>.yaml from the bundled " +
			"templates. The --kind flag selects the backend (run with " +
			"--help to see the full list); for kind=llama-server, " +
			"--frontend selects the rendering strategy (external | " +
			"docker-compose | managed). --frontend is ignored for every " +
			"other kind.\n\n" +
			"After the file is written, edit the REPLACE-marked lines, then " +
			"`vibe start <name>`. Refuses to overwrite an existing file " +
			"unless --force is passed.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			tmplName, err := templateForKindFrontend(kind, frontend)
			if err != nil {
				return err
			}
			return runProfileInit(cmd.OutOrStdout(), tmplName, name, hfRef, force)
		},
		ValidArgsFunction: cobra.NoFileCompletions,
	}
	cmd.Flags().StringVar(&kind, "kind", "llama-server",
		"backend kind: "+strings.Join(profileKinds(), " | "))
	cmd.Flags().StringVar(&frontend, "frontend", profile.FrontendExternal,
		"frontend kind (llama-server only): "+strings.Join(profileFrontendEnum, " | "))
	cmd.Flags().StringVar(&hfRef, "hf", "",
		"add a Hugging Face block to a llama-server profile (format: <repo>[:<file>])")
	cmd.Flags().BoolVar(&force, "force", false,
		"overwrite an existing profile file")

	// Tab-complete the enum flags so users discover the allowed values
	// without consulting --help.
	_ = cmd.RegisterFlagCompletionFunc("kind",
		func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			return profileKinds(), cobra.ShellCompDirectiveNoFileComp
		})
	_ = cmd.RegisterFlagCompletionFunc("frontend",
		func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			return profileFrontendEnum, cobra.ShellCompDirectiveNoFileComp
		})

	return cmd
}

func profileInitCmd() *cobra.Command {
	var (
		name  string
		hfRef string
		force bool
	)
	cmd := &cobra.Command{
		Use:        "init <kind>",
		Short:      "Deprecated alias for `vibe profile new` (kind as a positional).",
		Deprecated: "use `vibe profile new <name> --kind <kind>` instead.",
		Hidden:     true,
		Long: "Deprecated alias for `vibe profile new`, kept so existing scripts " +
			"keep working. It takes the kind as a positional argument and the " +
			"name via --name; `vibe profile new` is the canonical form (name " +
			"positional, kind via --kind, with tab-completion).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind := args[0]
			if name == "" {
				name = kind
			}
			return runProfileInit(cmd.OutOrStdout(), kind, name, hfRef, force)
		},
	}
	cmd.Flags().StringVar(&name, "name", "",
		"profile name (also the filename stem; defaults to <kind>)")
	cmd.Flags().StringVar(&hfRef, "hf", "",
		"add a HuggingFace block to a llama-server profile (format: <repo>[:<file>]; "+
			"e.g. Qwen/Qwen3-Coder-30B-A3B-Instruct-GGUF)")
	cmd.Flags().BoolVar(&force, "force", false,
		"overwrite an existing profile file")
	return cmd
}

// runProfileInit renders the starter template for `kind`, writes it to the
// user's profile directory, and prints a next-steps line. The write is
// refused if the destination exists and force is false.
func runProfileInit(out io.Writer, kind, name, hfRef string, force bool) error {
	if name == "" {
		return errors.New("--name must not be empty")
	}
	if !profileNameRE.MatchString(name) {
		return fmt.Errorf("--name %q must match [a-zA-Z0-9_-]+", name)
	}

	tmpl, err := loadProfileTemplate(kind)
	if err != nil {
		return err
	}

	body := strings.ReplaceAll(tmpl, profileNamePlaceholder, name)

	if hfRef != "" {
		if kind != "llama-server" {
			return fmt.Errorf("--hf is only valid for kind=llama-server (got %q)", kind)
		}
		body, err = injectHuggingFace(body, hfRef)
		if err != nil {
			return err
		}
	}

	dir := paths.ProfilesDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	dest := filepath.Join(dir, name+".yaml")
	if !force {
		if _, err := os.Stat(dest); err == nil {
			return fmt.Errorf("refusing to overwrite %s (pass --force to replace)", dest)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat %s: %w", dest, err)
		}
	}
	if err := os.WriteFile(dest, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", dest, err)
	}

	fmt.Fprintf(out, "Wrote %s\n", dest)
	fmt.Fprintf(out, "Next: edit the REPLACE-marked lines, then `vibe start %s`.\n", name)
	return nil
}

// injectHuggingFace edits a llama-server template body to add a
// `huggingface:` block under `backend.llama_server.path`. ref is parsed as
// `<repo>[:<file>]`; when the file is omitted, the user is expected to fill
// it in themselves (we leave a REPLACE marker).
//
// The function is intentionally a string-level patch rather than a YAML
// round-trip: the embedded template includes structured comments that the
// yaml.v3 marshaller would drop, and our test asserts those comments
// survive. The patch is anchored on the canonical `    path:` line emitted
// by the llama-server template.
func injectHuggingFace(body, ref string) (string, error) {
	repo, file, _ := strings.Cut(ref, ":")
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return "", fmt.Errorf("--hf %q: repo is required (format: <repo>[:<file>])", ref)
	}
	file = strings.TrimSpace(file)
	fileLine := "      file: REPLACE-<filename>.gguf  # REPLACE: pick the quant you want from the repo"
	if file != "" {
		fileLine = "      file: " + file
	}
	block := "    huggingface:\n" +
		"      repo: " + repo + "\n" +
		fileLine + "\n"

	// Anchor: the line `    path: ...` (4-space indent, llama_server child).
	// Insert the huggingface block immediately after the path line so the
	// resulting YAML is `path:` then `huggingface:` — matches the example
	// profiles' ordering.
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "    path:") {
			// Splice in: keep lines[:i+1], append block, then lines[i+1:].
			out := make([]string, 0, len(lines)+4)
			out = append(out, lines[:i+1]...)
			out = append(out, strings.TrimRight(block, "\n"))
			out = append(out, lines[i+1:]...)
			return strings.Join(out, "\n"), nil
		}
	}
	return "", errors.New("--hf: template missing `path:` anchor; cannot inject huggingface block")
}
