// Package bootstrap pins dependencies that components under construction will
// import once their packages exist.
//
// Temporary: pins dependencies for components under construction so go mod tidy
// keeps them; remove once packages import these directly.
package bootstrap

import (
	_ "github.com/anthropics/anthropic-sdk-go"
	_ "github.com/openai/openai-go/v3"
	_ "github.com/spf13/cobra"
	_ "google.golang.org/genai"
	_ "gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"
)
