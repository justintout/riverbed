// Package pipeline processes stored recordings.
//
// A worker claims a recording, routes it, embeds the transcription, and, when
// the route names an agent, runs that agent with the tools its configuration
// allows. Everything the run produced is written back: the decision, the reply,
// every tool call, and the tags.
//
// The queue is the recordings table, so a recording survives a restart and is
// retried rather than lost.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/justintout/riverbed/agent"
	"github.com/justintout/riverbed/config"
	"github.com/justintout/riverbed/embedding"
	"github.com/justintout/riverbed/route"
	"github.com/justintout/riverbed/store"
	"github.com/justintout/riverbed/tool"
)

// Options configures a Pipeline.
type Options struct {
	Store  *store.Store
	Router *route.Router
	// Agents are the configured agents by name.
	Agents map[string]agent.Agent
	// Tools is nil when no MCP server is configured.
	Tools *tool.Registry
	// ToolsFor reports which MCP servers an agent may use.
	ToolsFor func(agent string) []string
	// Embedder is nil when embedding is off.
	Embedder embedding.Embedder
	// Workers is the number of recordings processed at once.
	Workers int
	// MaxAttempts is how many times a recording is tried before it is failed.
	MaxAttempts int
	// IdlePoll is how often a worker looks for work without being notified.
	IdlePoll time.Duration
	Logger   *slog.Logger
}

// Pipeline processes recordings until its context is cancelled.
type Pipeline struct {
	opts Options
	log  *slog.Logger
	wake chan struct{}
}

// Defaults for Options.
const (
	DefaultWorkers     = 2
	DefaultMaxAttempts = 3
	DefaultIdlePoll    = 30 * time.Second
)

// New returns a pipeline.
func New(opts Options) (*Pipeline, error) {
	if opts.Store == nil {
		return nil, errors.New("pipeline: store is required")
	}
	if opts.Router == nil {
		return nil, errors.New("pipeline: router is required")
	}
	if opts.Workers <= 0 {
		opts.Workers = DefaultWorkers
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = DefaultMaxAttempts
	}
	if opts.IdlePoll <= 0 {
		opts.IdlePoll = DefaultIdlePoll
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Pipeline{
		opts: opts,
		log:  opts.Logger,
		// Buffered by one: a notification that arrives while a worker is busy
		// must not be lost, and several collapse into one wake-up.
		wake: make(chan struct{}, 1),
	}, nil
}

// Notify tells the workers that a recording is waiting. It never blocks.
func (p *Pipeline) Notify() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Run processes recordings until ctx is cancelled.
func (p *Pipeline) Run(ctx context.Context) error {
	// Work interrupted by a previous run is claimable again.
	if n, err := p.opts.Store.RequeueRunning(ctx); err != nil {
		return fmt.Errorf("pipeline: recover interrupted recordings: %w", err)
	} else if n > 0 {
		p.log.Info("recovered interrupted recordings", "count", n)
	}

	var wg sync.WaitGroup
	for i := 0; i < p.opts.Workers; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			p.work(ctx, worker)
		}(i)
	}
	wg.Wait()
	return ctx.Err()
}

// work drains the queue, then waits to be woken or for the idle poll.
func (p *Pipeline) work(ctx context.Context, worker int) {
	log := p.log.With("worker", worker)
	timer := time.NewTimer(p.opts.IdlePoll)
	defer timer.Stop()

	for {
		drained := false
		for !drained {
			select {
			case <-ctx.Done():
				return
			default:
			}

			rec, err := p.opts.Store.Claim(ctx)
			switch {
			case errors.Is(err, store.ErrNotFound):
				drained = true
			case err != nil:
				if ctx.Err() != nil {
					return
				}
				log.Error("claim recording", "error", err)
				drained = true
			default:
				p.process(ctx, rec)
			}
		}

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(p.opts.IdlePoll)

		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-timer.C:
		}
	}
}

// process handles one claimed recording.
func (p *Pipeline) process(ctx context.Context, rec store.Recording) {
	log := p.log.With("recording", rec.ID)
	started := time.Now()

	if err := p.handle(ctx, rec, log); err != nil {
		// A cancelled run means the process is stopping. Put the work back so
		// the next start picks it up.
		if ctx.Err() != nil {
			if err := p.opts.Store.Requeue(context.WithoutCancel(ctx), rec.ID); err != nil {
				log.Error("requeue on shutdown", "error", err)
			}
			return
		}
		if rec.Attempts < p.opts.MaxAttempts {
			log.Warn("processing failed, will retry",
				"error", err, "attempt", rec.Attempts, "of", p.opts.MaxAttempts)
			if err := p.opts.Store.Requeue(ctx, rec.ID); err != nil {
				log.Error("requeue after failure", "error", err)
			}
			return
		}
		log.Error("processing failed, giving up", "error", err, "attempts", rec.Attempts)
		if err := p.opts.Store.Finish(ctx, rec.ID, err); err != nil {
			log.Error("record failure", "error", err)
		}
		return
	}

	if err := p.opts.Store.Finish(ctx, rec.ID, nil); err != nil {
		log.Error("mark done", "error", err)
		return
	}
	log.Info("processed recording", "elapsed", time.Since(started).Round(time.Millisecond))
}

// handle routes, embeds and runs the agent for one recording.
func (p *Pipeline) handle(ctx context.Context, rec store.Recording, log *slog.Logger) error {
	decision := p.opts.Router.Route(ctx, rec.Transcription)
	log.Info("routed recording", "agent", decision.Agent, "reason", decision.Reason)

	if err := p.opts.Store.SetRoute(ctx, rec.ID, decision.Agent, decision.Reason, decision.Prompt); err != nil {
		return fmt.Errorf("record route: %w", err)
	}
	if err := p.opts.Store.AddTags(ctx, rec.ID, decision.Tags); err != nil {
		return fmt.Errorf("record tags: %w", err)
	}

	// Embedding happens whatever the route, because every recording belongs to
	// the searchable journal.
	if err := p.embed(ctx, rec); err != nil {
		// A failed embedding reduces retrieval quality but loses no data, so
		// it is logged instead of retried.
		log.Error("embed transcription", "error", err)
	}

	if decision.Journaled() {
		return nil
	}

	selected, ok := p.opts.Agents[decision.Agent]
	if !ok {
		return fmt.Errorf("agent %q is not configured", decision.Agent)
	}

	req := agent.Request{Prompt: decision.Prompt}
	if p.opts.Tools != nil && p.opts.ToolsFor != nil {
		servers := p.opts.ToolsFor(decision.Agent)
		if len(servers) > 0 {
			req.Tools = p.opts.Tools.Tools(ctx, servers)
			req.Invoke = p.opts.Tools.Call
		}
	}

	res, err := selected.Run(ctx, req)
	if err != nil {
		// The provider already names the agent, so it is not repeated here.
		return err
	}

	// Tool calls are stored even when the reply fails to store, because they
	// describe actions that already happened in the world.
	for _, call := range res.Calls {
		if err := p.opts.Store.AddToolCall(ctx, rec.ID, store.ToolCall{
			Server:    call.Server,
			Tool:      call.Tool,
			Arguments: call.Arguments,
			Result:    call.Result,
			IsError:   call.IsError,
			StartedAt: call.StartedAt,
			EndedAt:   call.EndedAt,
		}); err != nil {
			return fmt.Errorf("record tool call: %w", err)
		}
	}

	if err := p.opts.Store.AddResponse(ctx, rec.ID, store.Response{
		Agent:        decision.Agent,
		Text:         res.Text,
		InputTokens:  res.InputTokens,
		OutputTokens: res.OutputTokens,
	}); err != nil {
		return fmt.Errorf("record response: %w", err)
	}

	log.Info("agent answered",
		"agent", decision.Agent,
		"tool_calls", len(res.Calls),
		"input_tokens", res.InputTokens,
		"output_tokens", res.OutputTokens)
	return nil
}

// embed stores vectors for a recording's transcription.
func (p *Pipeline) embed(ctx context.Context, rec store.Recording) error {
	if p.opts.Embedder == nil || !p.opts.Store.VectorEnabled() || rec.Transcription == "" {
		return nil
	}
	chunks := embedding.Chunk(rec.Transcription, 0)
	if len(chunks) == 0 {
		return nil
	}
	vectors := make([][]float32, 0, len(chunks))
	for _, chunk := range chunks {
		v, err := p.opts.Embedder.Embed(ctx, chunk)
		if err != nil {
			return err
		}
		vectors = append(vectors, v)
	}
	return p.opts.Store.PutEmbeddings(ctx, rec.ID, chunks, vectors)
}

// Backfill embeds recordings stored before an embedder was configured. It stops
// when nothing is left or ctx is cancelled.
func (p *Pipeline) Backfill(ctx context.Context, batch int) (int, error) {
	if p.opts.Embedder == nil || !p.opts.Store.VectorEnabled() {
		return 0, nil
	}
	if batch <= 0 {
		batch = 100
	}

	total := 0
	for {
		ids, err := p.opts.Store.PendingEmbedding(ctx, batch)
		if err != nil {
			return total, err
		}
		if len(ids) == 0 {
			return total, nil
		}
		for _, id := range ids {
			if ctx.Err() != nil {
				return total, ctx.Err()
			}
			rec, err := p.opts.Store.Get(ctx, id)
			if err != nil {
				return total, err
			}
			if err := p.embed(ctx, rec); err != nil {
				return total, fmt.Errorf("embed recording %d: %w", id, err)
			}
			total++
		}
	}
}

// AgentNames returns the routable agent names, the journal included, for the
// classifier to choose from. The order is stable so the classifier prompt does
// not change between runs.
func AgentNames(agents map[string]agent.Agent) []string {
	names := make([]string, 0, len(agents))
	for name := range agents {
		names = append(names, name)
	}
	sort.Strings(names)
	return append([]string{config.JournalAgent}, names...)
}
