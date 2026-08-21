package neatlogs

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

const gracefulSignalTimeout = 30 * time.Second

// shutdownSignalController is installed only when Config.EnableSignalHandlers
// is true. It flushes and then exits with the conventional signal status. It
// does not re-deliver the signal: a host signal.Notify channel already receives
// the original delivery, and re-delivery would notify that host twice.
type shutdownSignalController struct {
	signals  chan os.Signal
	done     chan struct{}
	stopOnce sync.Once
}

func newShutdownSignalController() *shutdownSignalController {
	return &shutdownSignalController{
		signals: make(chan os.Signal, 1),
		done:    make(chan struct{}),
	}
}

func (c *shutdownSignalController) Start(shutdown func(os.Signal)) {
	signal.Notify(c.signals, os.Interrupt, syscall.SIGTERM)
	go c.run(shutdown, exitAfterSignal)
}

func (c *shutdownSignalController) run(shutdown func(os.Signal), terminate func(os.Signal)) {
	select {
	case sig := <-c.signals:
		// Stop our registration before flushing. Host-owned signal channels keep
		// their own registration and receive the original signal exactly once.
		c.Stop()
		shutdown(sig)
		terminate(sig)
	case <-c.done:
	}
}

func (c *shutdownSignalController) Stop() {
	c.stopOnce.Do(func() {
		signal.Stop(c.signals)
		close(c.done)
	})
}

func signalTerminationReason(sig os.Signal) string {
	switch sig {
	case os.Interrupt:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	default:
		return sig.String()
	}
}

func exitAfterSignal(sig os.Signal) {
	os.Exit(signalExitCode(sig))
}

func signalExitCode(sig os.Signal) int {
	switch sig {
	case os.Interrupt:
		return 130
	case syscall.SIGTERM:
		return 143
	default:
		return 1
	}
}
