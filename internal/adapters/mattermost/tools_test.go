package mattermost_test

import (
	"testing"

	"github.com/sarpaydemir/dost/internal/adapters/mattermost"
	"github.com/sarpaydemir/dost/internal/testkit/conformance"
)

func TestToolsKeepTheToolContract(t *testing.T) {
	conformance.Tools(t, mattermost.Tools(), mattermost.Prompts())
}
