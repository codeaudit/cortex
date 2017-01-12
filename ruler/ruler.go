package ruler

import (
	"fmt"
	"net/url"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/rules"
	"golang.org/x/net/context"

	"github.com/weaveworks/cortex/chunk"
	"github.com/weaveworks/cortex/distributor"
	"github.com/weaveworks/cortex/querier"
	"github.com/weaveworks/cortex/user"
)

var (
	evalDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: "cortex",
			Name:      "group_evaluation_duration_seconds",
			Help:      "The duration for a rule group to execute.",
		},
	)
	rulesProcessed = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "cortex",
		Name:      "rules_processed",
		Help:      "How many rules have been processed.",
	})
)

func init() {
	prometheus.MustRegister(evalDuration)
	prometheus.MustRegister(rulesProcessed)
}

// Config is the configuration for the recording rules server.
type Config struct {
	ConfigsAPIURL string
	ExternalURL   string
	// How frequently to evaluate rules by default.
	EvaluationInterval time.Duration
	NumWorkers         int
}

// Ruler evaluates rules.
type Ruler struct {
	Engine   *promql.Engine
	Appender SampleAppender
}

// NewRuler creates a new ruler from a distributor and chunk store.
func NewRuler(d *distributor.Distributor, c chunk.Store) Ruler {
	return Ruler{querier.NewEngine(d, c), d}
}

func (r *Ruler) getManagerOptions(ctx context.Context) *rules.ManagerOptions {
	appender := appenderAdapter{appender: r.Appender, ctx: ctx}
	return &rules.ManagerOptions{
		SampleAppender: appender,
		QueryEngine:    r.Engine,
		Context:        ctx,
	}
}

func (r *Ruler) newGroup(ctx context.Context, delay time.Duration, rs []rules.Rule) *rules.Group {
	return rules.NewGroup("default", delay, rs, r.getManagerOptions(ctx))
}

// Evaluate a list of rules in the given context.
func (r *Ruler) Evaluate(ctx context.Context, rs []rules.Rule) {
	log.Debugf("Evaluating %d rules...", len(rs))
	delay := 0 * time.Second // Unused, so 0 value is fine.
	start := time.Now()
	g := r.newGroup(ctx, delay, rs)
	g.Eval()
	// The prometheus routines we're calling have their own instrumentation
	// but, a) it's rule-based, not group-based, b) it's a summary, not a
	// histogram, so we can't reliably aggregate.
	evalDuration.Observe(time.Since(start).Seconds())
	rulesProcessed.Add(float64(len(rs)))
}

type server struct {
	scheduler *scheduler
	workers   []worker
}

// NewServer makes a new rule processing server.
func NewServer(cfg Config, ruler Ruler) (Worker, error) {
	configsAPIURL, err := url.Parse(cfg.ConfigsAPIURL)
	if err != nil {
		return nil, err
	}
	configsAPI := configsAPI{configsAPIURL}
	delay := time.Duration(cfg.EvaluationInterval)
	// TODO: Separate configuration for polling interval.
	scheduler := newScheduler(configsAPI, delay, delay)
	if cfg.NumWorkers <= 0 {
		return nil, fmt.Errorf("Must have at least 1 worker, got %d", cfg.NumWorkers)
	}
	workers := make([]worker, cfg.NumWorkers)
	for i := 0; i < cfg.NumWorkers; i++ {
		workers[i] = newWorker(&scheduler, ruler)
	}
	return &server{
		scheduler: &scheduler,
		workers:   workers,
	}, nil
}

func (s *server) Run() {
	go s.scheduler.Run()
	for _, w := range s.workers {
		go w.Run()
	}
	log.Infof("Ruler up and running")
}

func (s *server) Stop() {
	for _, w := range s.workers {
		w.Stop()
	}
	s.scheduler.Stop()
}

// Worker does a thing until it's told to stop.
type Worker interface {
	Run()
	Stop()
}

type worker struct {
	scheduler *scheduler
	ruler     Ruler

	done       chan struct{}
	terminated chan struct{}
}

func newWorker(scheduler *scheduler, ruler Ruler) worker {
	return worker{
		scheduler: scheduler,
		ruler:     ruler,
	}
}

func (w *worker) Run() {
	defer close(w.terminated)
	for {
		select {
		case <-w.done:
			return
		default:
		}
		item := w.scheduler.nextWorkItem(time.Now())
		if item == nil {
			log.Debugf("Queue closed. Terminating worker.")
			return
		}
		ctx := user.WithID(context.Background(), item.userID)
		w.ruler.Evaluate(ctx, item.rules)
		// XXX: Should we have some sort of small delay / yielding point here
		// to prevent monopolising the CPU?
	}
}

func (w *worker) Stop() {
	close(w.done)
	<-w.terminated
}
