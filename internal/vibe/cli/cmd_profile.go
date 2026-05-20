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
	"sort"
	"strings"

	"github.com/spf13/cobra"

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
	return cmd
}

// profileKindEnum is the user-facing --kind values for `vibe profile new`.
// It deliberately mirrors the backend discriminators in profile.Backend
// (llama_server, comfyui) using kebab-case so the CLI matches `vibe`'s
// flag-naming conventions; the internal mapping to template filenames
// happens in templateForKindFrontend.
var profileKindEnum = []string{"llama-server", "comfyui"}

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
// concrete template filename under profile_templates/. The mapping
// recognizes that the existing templates are organized by frontend kind
// for llama-server (one template per frontend variant) but by backend
// for comfyui (a single template, frontend ignored). Future kinds plug
// in by extending this switch.
func templateForKindFrontend(kind, frontend string) (string, error) {
	switch kind {
	case "llama-server":
		switch frontend {
		case profile.FrontendExternal:
			return "llama-server", nil
		case profile.FrontendDockerCompose:
			return "docker-compose", nil
		case profile.FrontendManaged:
			return "managed", nil
		default:
			return "", fmt.Errorf("--frontend %q: unknown (allowed: %s)",
				frontend, strings.Join(profileFrontendEnum, ", "))
		}
	case "comfyui":
		// Frontend is irrelevant for comfyui (the backend ships its own UI),
		// so we accept any value and route to the comfyui template. The
		// alternative — erroring on --frontend != "" — would be surprising
		// when users tab-complete the default. The validator in profile
		// rejects frontend blocks for comfyui profiles, so there's no
		// hidden risk in accepting the flag here.
		return "comfyui", nil
	default:
		return "", fmt.Errorf("--kind %q: unknown (allowed: %s)",
			kind, strings.Join(profileKindEnum, ", "))
	}
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
			"templates. The --kind flag selects the backend " +
			"(llama-server | comfyui); --frontend selects the rendering " +
			"strategy for llama-server (external | docker-compose | " +
			"managed) and is ignored for comfyui (which ships its own UI).\n\n" +
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
		"backend kind: "+strings.Join(profileKindEnum, " | "))
	cmd.Flags().StringVar(&frontend, "frontend", profile.FrontendExternal,
		"frontend kind (llama-server only): "+strings.Join(profileFrontendEnum, " | "))
	cmd.Flags().StringVar(&hfRef, "hf", "",
		"add a HuggingFace block to a llama-server profile (format: <repo>[:<file>])")
	cmd.Flags().BoolVar(&force, "force", false,
		"overwrite an existing profile file")

	// Tab-complete the enum flags so users discover the allowed values
	// without consulting --help.
	_ = cmd.RegisterFlagCompletionFunc("kind",
		func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			return profileKindEnum, cobra.ShellCompDirectiveNoFileComp
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
		Use:   "init <kind>",
		Short: "Drop a starter profile YAML in $XDG_CONFIG_HOME/vibe/profiles/.",
		Long: "Drops a starter profile YAML in $XDG_CONFIG_HOME/vibe/profiles/ so " +
			"new users don't have to copy-paste from the examples directory.\n\n" +
			"The dropped file has `# REPLACE: ...` markers on fields the user must " +
			"edit (model path, alias, ComfyUI directory, etc.). After editing those " +
			"lines, `vibe start <name>` activates the profile.\n\n" +
			"Refuses to overwrite an existing file unless --force is passed.",
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
