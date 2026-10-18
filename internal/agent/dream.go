package agent

import (
	"context"

	"github.com/yedamao/nanobot-go/internal/memory"
	"github.com/yedamao/nanobot-go/internal/tools"
)

// DreamChannel is the channel name Dream turns run under.
const DreamChannel = "dream"

// DreamRun adapts r to memory.DreamRunFunc, like nanobot's
// process_direct(..., ephemeral=True, tools=...): each call is one turn
// with the normal system prompt (so the model sees SOUL.md, USER.md and
// memory/MEMORY.md), no replayed history and no session, offering only
// the registry Dream passes in. spec supplies the model, limits and hooks;
// its Messages and Tools are replaced.
func DreamRun(r *Runner, cb *ContextBuilder, spec RunSpec) memory.DreamRunFunc {
	return func(ctx context.Context, prompt string, reg *tools.Registry) (memory.DreamOutcome, error) {
		spec := spec
		spec.Messages = cb.BuildMessages("", nil, prompt, DreamChannel)
		spec.Tools = reg
		res, err := r.Run(ctx, spec)
		if res == nil {
			return memory.DreamOutcome{}, err
		}
		return memory.DreamOutcome{
			Completed:  err == nil && res.StopReason == StopCompleted,
			StopReason: res.StopReason,
			Reply:      res.FinalContent,
		}, err
	}
}
