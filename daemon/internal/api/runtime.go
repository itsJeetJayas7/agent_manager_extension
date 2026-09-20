package api

import (
	"context"
	"runtime"
)

func contextBG() context.Context { return context.Background() }

func runtimeGOOS() string { return runtime.GOOS }
