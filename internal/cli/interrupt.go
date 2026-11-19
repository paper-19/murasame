package cli

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"

	"murasame/internal/engine"
	"murasame/internal/states"
)

var exit = os.Exit

var brokenPipes sync.Once

func surviveBrokenPipes() {
	brokenPipes.Do(func() {
		signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
	})
}

type interruption struct {
	why     atomic.Pointer[string]
	mu      sync.Mutex
	run     *states.Tracker[states.RunStop, states.RunStopEvent]
	dryRun  bool
	target  string
	cleanup func()
}

func newInterruption(dryRun bool) *interruption {
	return &interruption{run: states.RunStopMachine.Start(), dryRun: dryRun}
}

func (i *interruption) fire(ev states.RunStopEvent) (states.RunStop, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.run.Fire(ev); err != nil {
		return i.run.State(), false
	}
	return i.run.State(), true
}

func (i *interruption) State() states.RunStop {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.run.State()
}

func (i *interruption) Begin(target string) {
	i.mu.Lock()
	i.target = target
	i.mu.Unlock()
	i.fire(states.RunBegins)
}

func (i *interruption) Locked(held bool) {
	switch {
	case i.dryRun:
	case held:
		i.fire(states.RunLocks)
	default:
		i.fire(states.RunUnlocks)
	}
}

func (i *interruption) Throwaway(cleanup func()) {
	i.mu.Lock()
	i.cleanup = cleanup
	i.mu.Unlock()
}

func (i *interruption) Gating([]engine.Item, string) {}

func (i *interruption) Outcome(engine.Item, engine.Outcome) {}

func (i *interruption) Poll() string {
	if p := i.why.Load(); p != nil {
		return *p
	}
	return ""
}

func signalName(s os.Signal) string {
	if s == syscall.SIGTERM {
		return "SIGTERM"
	}
	if s == syscall.SIGINT {
		return "SIGINT"
	}
	if s == syscall.SIGHUP {
		return "SIGHUP"
	}
	return s.String()
}

func (i *interruption) signalled(s os.Signal, stderr io.Writer) bool {
	to, ok := i.fire(states.RunSignal)
	if !ok {
		return false
	}
	switch to {
	case states.RunAbandoned:
		fmt.Fprintf(stderr, "murasame: %s: stopped before the queue started\n", signalName(s))
		i.mu.Lock()
		cleanup := i.cleanup
		i.mu.Unlock()
		if cleanup != nil {
			cleanup()
		}
		exit(1)
		return false
	case states.RunSignalled:
		why := "interrupted by " + signalName(s)
		i.why.Store(&why)
		i.mu.Lock()
		target := i.target
		i.mu.Unlock()
		fmt.Fprintf(stderr, "murasame: %s: stopping the running gate or command; integ goes back to %s "+
			"(a second signal stops at once)\n", signalName(s), target)
	case states.RunSecondSignal:
		fmt.Fprintln(stderr, "murasame: stopped at once (a second signal): a running gate may be left behind")
		exit(1)
		return false
	}
	return true
}

func stopOnSignals(it *interruption, stderr io.Writer) func() {
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case s := <-sig:
				if !it.signalled(s, stderr) {
					return
				}
			case <-done:
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			it.fire(states.RunReturns)
			signal.Stop(sig)
			close(done)
			<-finished
			it.fire(states.RunStops)
		})
	}
}
