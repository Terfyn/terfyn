package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/Terfyn/terfyn/internal/audit"
	"github.com/Terfyn/terfyn/internal/render"
	"github.com/Terfyn/terfyn/internal/state"
	"github.com/Terfyn/terfyn/internal/state/sqlite"
	"github.com/spf13/cobra"
)

func newAuditCmd() *cobra.Command {
	var runID string
	var limit int

	cmd := &cobra.Command{
		Use:          "audit",
		Short:        "Verify tamper-evident trace audit chains",
		SilenceUsage: true,
		Long: `Verify hash-linked trace event chains stored in the SQLite state database.

Each run's trace_events rows form a per-run chain: event hash covers canonical
(redacted) fields plus the previous event hash. Pre-migration rows without hashes
are reported as unchained and do not fail verification.

Without --run, verifies recent runs only (default --limit 50, max 500).

Examples:
  terfyn audit verify
  terfyn audit verify --run <run-id>
  terfyn audit verify --limit 200

Exit codes:
  0 — all checked chains valid (unchained rows allowed)
  1 — chain break detected or operational failure
  2 — validation failure (unknown run id)`,
	}
	verify := &cobra.Command{
		Use:          "verify",
		Short:        "Re-derive trace hashes and detect chain breaks",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = args
			return runAuditVerify(cmd, runID, limit)
		},
	}
	verify.Flags().StringVar(&runID, "run", "", "verify only this run id")
	verify.Flags().IntVar(&limit, "limit", state.DefaultRunListLimit, "max runs to verify when --run is omitted")
	cmd.AddCommand(verify)
	return cmd
}

type auditVerifyRecord struct {
	RunID     string `json:"runId"`
	OK        bool   `json:"ok"`
	Total     int    `json:"total"`
	Chained   int    `json:"chained"`
	Unchained int    `json:"unchained"`
	BrokenSeq int64  `json:"brokenSeq,omitempty"`
	BrokenAt  string `json:"brokenAt,omitempty"`
}

func runAuditVerify(cmd *cobra.Command, runID string, limit int) error {
	ctx := context.Background()
	g := Globals()
	runID = strings.TrimSpace(runID)

	graph, root, err := prepareProjectGraph(g)
	if err != nil {
		return NewExitError(ExitValidationError, err)
	}
	dsn, err := resolveStateSQLitePath(root, graph, g.StatePath)
	if err != nil {
		return fmt.Errorf("audit verify: resolve state path: %w", err)
	}

	st, err := sqlite.OpenReadOnly(ctx, dsn)
	if err != nil {
		return fmt.Errorf("audit verify: open sqlite %q: %w", dsn, err)
	}
	defer func() { _ = st.Close() }()

	var runIDs []string
	if runID != "" {
		if _, err := st.GetRun(ctx, runID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return NewExitErrorf(ExitValidationError, "audit verify: unknown run %q", runID)
			}
			return fmt.Errorf("audit verify: get run: %w", err)
		}
		runIDs = []string{runID}
	} else {
		runs, err := st.ListRecentRuns(ctx, state.ClampRunListLimit(limit))
		if err != nil {
			return fmt.Errorf("audit verify: list runs: %w", err)
		}
		runIDs = make([]string, 0, len(runs))
		for _, r := range runs {
			runIDs = append(runIDs, r.RunID)
		}
	}

	records := make([]auditVerifyRecord, 0, len(runIDs))
	allOK := true
	for _, id := range runIDs {
		events, err := st.ListTraceEventsByRunID(ctx, id)
		if err != nil {
			return fmt.Errorf("audit verify: list trace events for %q: %w", id, err)
		}
		res := audit.VerifyRunChain(id, events)
		rec := auditVerifyRecord{
			RunID:     id,
			OK:        res.Ok(),
			Total:     res.Total,
			Chained:   res.Chained,
			Unchained: res.Unchained,
		}
		if !res.Ok() {
			allOK = false
			rec.BrokenSeq = res.BrokenSeq
			rec.BrokenAt = res.BrokenField
		}
		records = append(records, rec)
	}

	out := cmd.OutOrStdout()
	switch g.Output {
	case render.FormatJSON:
		payload := struct {
			StatePath string              `json:"statePath"`
			OK        bool                `json:"ok"`
			Runs      []auditVerifyRecord `json:"runs"`
		}{StatePath: dsn, OK: allOK, Runs: records}
		if err := render.WriteJSON(out, payload); err != nil {
			return err
		}
	case render.FormatYAML:
		payload := struct {
			StatePath string              `json:"statePath"`
			OK        bool                `json:"ok"`
			Runs      []auditVerifyRecord `json:"runs"`
		}{StatePath: dsn, OK: allOK, Runs: records}
		if err := render.WriteYAML(out, payload); err != nil {
			return err
		}
	default:
		fmt.Fprintf(out, "State: %s\n", dsn)
		for _, rec := range records {
			if rec.OK {
				fmt.Fprintf(out, "run %s: OK (%d chained, %d unchained)\n", rec.RunID, rec.Chained, rec.Unchained)
				continue
			}
			fmt.Fprintf(out, "run %s: BROKEN at seq %d (%s)\n", rec.RunID, rec.BrokenSeq, rec.BrokenAt)
		}
	}

	if !allOK {
		return NewExitErrorf(ExitGenericFailure, "audit verify: one or more trace chains are broken")
	}
	return nil
}
