//go:build faultinject

package faultinject

import (
	"fmt"
	"os"
	"strings"
)

func CrashAt(point string) {
	for _, configured := range strings.Split(os.Getenv(EnvCrashPoint), ",") {
		if strings.TrimSpace(configured) == point {
			fmt.Fprintf(os.Stderr, "FAULT INJECTION: process crashing at %s\n", point)
			os.Exit(CrashExitCode)
		}
	}
}
