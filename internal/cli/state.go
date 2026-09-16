package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Terfyn/terfyn/internal/render"
	"github.com/Terfyn/terfyn/internal/state"
	"github.com/Terfyn/terfyn/internal/state/sqlite"
	"github.com/Terfyn/terfyn/internal/statejson"
	"github.com/spf13/cobra"
)

// maxStateJSONSnippet is the max runes shown for normalized_spec_json in table output (issue #72).
const maxStateJSONSnippet = 120

func newStateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "state",
		Short: "Inspect SQLite deployment state (applied resources)",
		Long: `Read-only commands for rows in the deployment store (design doc §10.2, §14.1).

Uses the same --project / --state resolution as plan and apply. Rows are scoped to the
environment from -e / --env when set, otherwise "local".

This command does not modify state.`,
	}
	cmd.AddCommand(newStateListCmd())
	cmd.AddCommand(newStateShowCmd())
	return cmd
}

func newStateListCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "list",
		Short:        "List applied resources for the selected environment",
		SilenceUsage: true,
		Long: `Print every row in applied_resources for the current environment, plus the
applied_projects row for this project when present.

Exit codes (§11.2): 0 success, 1 SQLite failure, 2 validation failure (invalid project).`,
		RunE: runStateList,
	}
}

func newStateShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "show Kind/name",
		Short:        "Show one applied resource row",
		SilenceUsage: true,
		Long: `Print kind, name, environment, spec hash, applied time, and normalized spec JSON
for one applied_resources row. Kind/name parsing matches other commands (case-insensitive kind).

For large JSON, table output truncates with "..."; use -o json or -o yaml for the full value.

Exit codes (§11.2): 0 success, 1 SQLite failure, 2 validation or unknown resource.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return NewExitError(ExitValidationError, fmt.Errorf("state: show requires exactly one Kind/name argument"))
			}
			return runStateShow(cmd, args)
		},
	}
}

func runStateList(cmd *cobra.Command, args []string) error {
	_ = args
	ctx := context.Background()
	g := Globals()

	graph, root, err := prepareProjectGraph(g)
	if err != nil {
		return NewExitError(ExitValidationError, err)
	}
	env := planEnvironment(g)
	dsn, err := resolveStateSQLitePath(root, graph, g.StatePath)
	if err != nil {
		return fmt.Errorf("state: resolve state path: %w", err)
	}

	st, err := sqlite.OpenReadOnly(ctx, dsn)
	if err != nil {
		return fmt.Errorf("state: open sqlite %q: %w", dsn, err)
	}
	defer func() { _ = st.Close() }()

	rows, err := st.ListAppliedResourcesByEnv(ctx, env)
	if err != nil {
		return fmt.Errorf("state: list applied resources: %w", err)
	}

	var proj *state.AppliedProject
	pname := strings.TrimSpace(graph.Meta.Name)
	if pname != "" {
		p, err := st.GetAppliedProject(ctx, env, pname)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("state: get applied project: %w", err)
		}
		if err == nil {
			proj = p
		}
	}

	return writeStateListOutput(cmd, g, env, dsn, proj, rows)
}

func runStateShow(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	g := Globals()

	id, err := ParseResourceRef(args[0])
	if err != nil {
		return NewExitError(ExitValidationError, fmt.Errorf("state: %w", err))
	}

	graph, root, err := prepareProjectGraph(g)
	if err != nil {
		return NewExitError(ExitValidationError, err)
	}
	env := planEnvironment(g)
	dsn, err := resolveStateSQLitePath(root, graph, g.StatePath)
	if err != nil {
		return fmt.Errorf("state: resolve state path: %w", err)
	}

	st, err := sqlite.OpenReadOnly(ctx, dsn)
	if err != nil {
		return fmt.Errorf("state: open sqlite %q: %w", dsn, err)
	}
	defer func() { _ = st.Close() }()

	row, err := st.GetAppliedResource(ctx, env, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return NewExitError(ExitValidationError, fmt.Errorf("state: no applied resource %s for environment %q", id.String(), env))
		}
		return fmt.Errorf("state: get applied resource: %w", err)
	}

	return writeStateShowOutput(cmd, g, dsn, env, row)
}

func writeStateListOutput(cmd *cobra.Command, g *Global, env, dsn string, proj *state.AppliedProject, rows []state.AppliedResource) error {
	out := cmd.OutOrStdout()
	switch g.Output {
	case render.FormatJSON:
		return render.WriteJSON(out, statejson.StateListPayload{
			Environment: env, StatePath: dsn,
			Resources:      statejson.AppliedResources(rows),
			AppliedProject: statejson.AppliedProject(proj),
		})
	case render.FormatYAML:
		return render.WriteYAML(out, statejson.StateListPayload{
			Environment: env, StatePath: dsn,
			Resources:      statejson.AppliedResources(rows),
			AppliedProject: statejson.AppliedProject(proj),
		})
	default:
		if _, err := fmt.Fprintf(out, "Environment: %s\nState: %s\n\n", env, dsn); err != nil {
			return err
		}
		if proj != nil {
			if _, err := fmt.Fprintf(out, "Applied project: %s  version=%s  appliedAt=%s\n\n",
				proj.ProjectName, proj.Version, proj.AppliedAt.UTC().Format(time.RFC3339)); err != nil {
				return err
			}
		}
		if len(rows) == 0 {
			_, err := fmt.Fprintf(out, "No applied resources for environment %q.\n", env)
			return err
		}
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "KIND\tNAME\tSPEC_HASH\tAPPLIED_AT")
		for _, r := range rows {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Kind, r.Name, r.SpecHash, r.AppliedAt.UTC().Format(time.RFC3339))
		}
		return tw.Flush()
	}
}

func writeStateShowOutput(cmd *cobra.Command, g *Global, dsn, env string, row *state.AppliedResource) error {
	if row == nil {
		return fmt.Errorf("state: nil row")
	}
	out := cmd.OutOrStdout()
	switch g.Output {
	case render.FormatJSON:
		return render.WriteJSON(out, struct {
			Environment string                          `json:"environment"`
			StatePath   string                          `json:"statePath"`
			Resource    statejson.AppliedResourceRecord `json:"resource"`
		}{env, dsn, statejson.AppliedResource(*row)})
	case render.FormatYAML:
		return render.WriteYAML(out, struct {
			Environment string                          `json:"environment"`
			StatePath   string                          `json:"statePath"`
			Resource    statejson.AppliedResourceRecord `json:"resource"`
		}{env, dsn, statejson.AppliedResource(*row)})
	default:
		if _, err := fmt.Fprintf(out, "Environment: %s\nState: %s\n\n", env, dsn); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(out, "Kind:        %s\nName:        %s\nEnv:         %s\nSpec hash:   %s\nApplied at:  %s\n\n",
			row.Kind, row.Name, row.Env, row.SpecHash, row.AppliedAt.UTC().Format(time.RFC3339)); err != nil {
			return err
		}
		if _, err := fmt.Fprint(out, "Normalized spec (truncated in table output; use -o json or yaml for full JSON):\n"); err != nil {
			return err
		}
		_, err := fmt.Fprint(out, clipString(row.NormalizedSpecJSON, maxStateJSONSnippet)+"\n")
		return err
	}
}

func clipString(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if maxRunes <= 0 || s == "" {
		return s
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "..."
}
