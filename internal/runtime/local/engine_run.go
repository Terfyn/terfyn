package local

import (
	"github.com/Terfyn/terfyn/internal/runtime"
)

// engineRunConfig carries execution options shared by Invoke and Resume engine paths.
type engineRunConfig struct {
	approvedActions []string
	autoApprove     bool
	hitlActor       string
	hitlDecision    *runtime.HitlDecisionOptions
	traceDetail     bool
}

func engineRunConfigFromInvoke(opts runtime.InvokeOptions) engineRunConfig {
	return engineRunConfig{
		approvedActions: opts.ApprovedActions,
		autoApprove:     opts.AutoApprove,
		hitlActor:       opts.HitlActor,
		traceDetail:     opts.TraceDetail,
	}
}

func engineRunConfigFromResume(opts runtime.ResumeOptions) engineRunConfig {
	return engineRunConfig{
		approvedActions: opts.ApprovedActions,
		autoApprove:     opts.AutoApprove,
		hitlActor:       opts.HitlActor,
		hitlDecision:    opts.HitlDecision,
		traceDetail:     opts.TraceDetail,
	}
}
