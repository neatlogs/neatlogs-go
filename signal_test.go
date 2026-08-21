package neatlogs

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const (
	signalHelperMode = "NEATLOGS_SIGNAL_HELPER_MODE"
	signalHelperFile = "NEATLOGS_SIGNAL_HELPER_FILE"
)

func TestSignalHandlersAreOptInInChildProcess(t *testing.T) {
	for _, test := range []struct {
		name    string
		mode    string
		flushed bool
		want    func(t *testing.T, status syscall.WaitStatus)
	}{
		{
			name:    "zero-value-keeps-default-signal",
			mode:    "default",
			flushed: false,
			want: func(t *testing.T, status syscall.WaitStatus) {
				if !status.Signaled() || status.Signal() != syscall.SIGTERM {
					t.Fatalf("status = %v, want termination by SIGTERM", status)
				}
			},
		},
		{
			name:    "explicit-opt-in-gracefully-exits",
			mode:    "enabled",
			flushed: true,
			want: func(t *testing.T, status syscall.WaitStatus) {
				if status.Signaled() || status.ExitStatus() != signalExitCode(syscall.SIGTERM) {
					t.Fatalf("status = %v, want exit %d", status, signalExitCode(syscall.SIGTERM))
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			flushFile := t.TempDir() + "/shutdown"
			cmd := exec.Command(os.Args[0], "-test.run=^TestSignalHandlerChild$")
			cmd.Env = append(os.Environ(), signalHelperMode+"="+test.mode, signalHelperFile+"="+flushFile)
			err := cmd.Run()
			if err == nil {
				t.Fatal("child unexpectedly exited successfully")
			}
			exitErr, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("child error = %T %v", err, err)
			}
			status, ok := exitErr.ProcessState.Sys().(syscall.WaitStatus)
			if !ok {
				t.Fatalf("process status = %T", exitErr.ProcessState.Sys())
			}
			test.want(t, status)
			_, statErr := os.Stat(flushFile)
			if test.flushed && statErr != nil {
				t.Fatalf("signal shutdown did not close exporter: %v", statErr)
			}
			if !test.flushed && !os.IsNotExist(statErr) {
				t.Fatalf("zero-value signal handling unexpectedly shut down exporter: %v", statErr)
			}
		})
	}
}

func TestSignalHandlerChild(t *testing.T) {
	mode := os.Getenv(signalHelperMode)
	if mode == "" {
		return
	}
	ctx := context.Background()
	shutdown, err := Init(ctx, Config{
		EnableSignalHandlers: mode == "enabled",
	}, WithExporter(&signalFileExporter{path: os.Getenv(signalHelperFile)}))
	if err != nil {
		os.Exit(97)
	}
	defer shutdown(ctx)
	process, err := os.FindProcess(os.Getpid())
	if err != nil || process.Signal(syscall.SIGTERM) != nil {
		os.Exit(98)
	}
	time.Sleep(5 * time.Second)
	os.Exit(99)
}

type signalFileExporter struct {
	path string
}

func (*signalFileExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }

func (e *signalFileExporter) Shutdown(context.Context) error {
	return os.WriteFile(e.path, []byte("shutdown"), 0o600)
}

func TestHostSignalChannelReceivesNoDuplicateRedelivery(t *testing.T) {
	host := make(chan os.Signal, 2)
	signal.Notify(host, syscall.SIGTERM)
	defer signal.Stop(host)

	controller := newShutdownSignalController()
	signal.Notify(controller.signals, syscall.SIGTERM)
	defer controller.Stop()
	terminated := make(chan os.Signal, 1)
	go controller.run(func(os.Signal) {}, func(sig os.Signal) { terminated <- sig })

	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	select {
	case sig := <-host:
		if sig != syscall.SIGTERM {
			t.Fatalf("host received %v, want SIGTERM", sig)
		}
	case <-time.After(time.Second):
		t.Fatal("host did not receive the original signal")
	}
	select {
	case <-terminated:
	case <-time.After(time.Second):
		t.Fatal("controller did not terminate after shutdown")
	}
	select {
	case duplicate := <-host:
		t.Fatalf("host received duplicate re-delivery %v", duplicate)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSignalConfigZeroValueAndDeprecatedDisableDoNotInstallController(t *testing.T) {
	ctx := context.Background()
	for _, cfg := range []Config{
		{DisableExport: true},
		{DisableExport: true, EnableSignalHandlers: true, DisableSignalHandlers: true},
	} {
		shutdown, err := Init(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		global.mu.Lock()
		controller := global.runtime.signals
		global.mu.Unlock()
		if controller != nil {
			t.Fatal("signal controller installed without unambiguous opt-in")
		}
		if err := shutdown(ctx); err != nil {
			t.Fatal(err)
		}
	}
	client, err := NewClient(ctx, Config{DisableExport: true, EnableSignalHandlers: true})
	if err != nil {
		t.Fatal(err)
	}
	if client.runtime.signals != nil {
		t.Fatal("context-scoped Client installed a process signal controller")
	}
	if err := client.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
