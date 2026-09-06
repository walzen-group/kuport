//go:build tools

// Package tools pins project-wide dependencies at the versions the whole repo
// builds against. The controller, the agent, and the command (later tasks)
// import these; this file keeps them in go.mod so `go mod tidy` cannot drop the
// pins before that code exists. The build tag excludes it from every build.
package tools

import (
	_ "k8s.io/api/core/v1"
	_ "sigs.k8s.io/controller-runtime"
)
