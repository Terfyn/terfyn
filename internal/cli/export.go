package cli

import (
	"fmt"

	"github.com/Terfyn/terfyn/internal/project"
	"github.com/spf13/cobra"
)

func newExportCmd() *cobra.Command {
	var format, output string
	cmd := &cobra.Command{
		Use:          "export",
		Short:        "Materialize the compiled resource graph as YAML",
		SilenceUsage: true,
		Long: `Compile the project (.agent authoring surface) into its resource graph and
materialize it — as a one-way YAML stream for inspection, or as a loadable .agent project
directory (ADR 003 / ADR 007).

By default the graph is written to stdout as a multi-document YAML stream for inspection or
handoff. That YAML is NOT the trustworthy record (applied deployment state plus the audit
chain is) and, under ADR 007, is NOT a project source — validate/plan/apply/run refuse a
project.yaml.

Pass --output DIR to write a loadable project instead. Because .agent is the sole executable
source under ADR 007, the directory is a consolidated project.agent (plus a schemas/ directory
holding each resolved agent input/output and workflow input schema), so
'terfyn validate/plan/apply/run --project DIR' works. DIR is treated as generated output: its
schemas/ directory is replaced, and a directory that already contains a foreign .agent source
is refused. Some things are not preserved (inherited raise/ADR-007 limitations, the same as
'terfyn migrate --to-agent', not new): the project's metadata.name (.agent has no project-name
form; a reloaded project is named after DIR), a workflow's declared return type (-> Type is not
carried in the graph, so the reloaded workflow is untyped on its output), and a workflow's
'parallel' concurrency (raise linearizes the step DAG, so parallel steps reload as a chain).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExport(cmd, format, output)
		},
	}
	cmd.Flags().StringVar(&format, "format", "yaml", "stdout serialization format (yaml); ignored with --output, which writes a .agent project")
	cmd.Flags().StringVar(&output, "output", "", "write a loadable .agent project directory instead of printing the YAML stream to stdout")
	return cmd
}

func runExport(cmd *cobra.Command, format, output string) error {
	// --format selects the stdout serialization; it is ignored when --output writes a .agent project.
	if output == "" && format != "yaml" {
		return NewExitErrorf(ExitValidationError, "unsupported export format %q (only \"yaml\" is supported)", format)
	}
	graph, _, err := prepareProjectGraph(Globals())
	if err != nil {
		return NewExitError(ExitValidationError, err)
	}

	if output == "" {
		data, err := project.ExportYAML(graph)
		if err != nil {
			return NewExitError(ExitGenericFailure, err)
		}
		_, err = cmd.OutOrStdout().Write(data)
		return err
	}

	if err := project.WriteAgentProjectDir(output, graph); err != nil {
		return NewExitError(ExitGenericFailure, err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "exported .agent project to %s\n", output)
	return nil
}
