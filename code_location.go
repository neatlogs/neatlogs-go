package neatlogs

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"go.opentelemetry.io/otel/attribute"
)

var codeLocationHome, _ = os.UserHomeDir()

func manualSpanCodeLocation() []attribute.KeyValue {
	var pcs [1]uintptr
	// Skip runtime.Callers, this helper, startTypedSpan, and Trace or StartSpan.
	if runtime.Callers(4, pcs[:]) == 0 {
		return nil
	}
	frame, _ := runtime.CallersFrames(pcs[:]).Next()
	if frame.File == "" || frame.Line <= 0 {
		return nil
	}

	file := frame.File
	if codeLocationHome != "" {
		if rel, err := filepath.Rel(codeLocationHome, file); err == nil && filepath.IsLocal(rel) {
			file = rel
		}
	}
	location := []attribute.KeyValue{
		attribute.String("neatlogs.code.file.path", filepath.ToSlash(file)),
		attribute.Int("neatlogs.code.line.number", frame.Line),
	}
	// The first dot after the last slash separates the package from the symbol.
	start := strings.LastIndexByte(frame.Function, '/') + 1
	if offset := strings.IndexByte(frame.Function[start:], '.'); offset >= 0 {
		index := start + offset
		location = append(location,
			attribute.String("neatlogs.code.function.name", frame.Function[index+1:]),
			attribute.String("neatlogs.code.namespace", frame.Function[:index]),
		)
	}
	return location
}
